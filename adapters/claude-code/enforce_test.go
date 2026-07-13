package claudecode

import (
	"bytes"
	"context"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/sidecar"
)

// countingListener wraps a net.Listener and counts accepted connections, so a
// test can assert whether the enforce hook actually DIALED the sidecar.
type countingListener struct {
	net.Listener
	n *int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		atomic.AddInt32(l.n, 1)
	}
	return c, err
}

// serveSidecar starts a real sidecar.Server on a temp Unix socket (optionally with
// bundle b) and returns the socket path plus an accept counter. The daemon is
// torn down on test cleanup.
func serveSidecar(t *testing.T, b *sidecar.Bundle) (socket string, accepts *int32) {
	t.Helper()
	socket = filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	var n int32
	srv := sidecar.NewServer(sidecar.ServerConfig{})
	if b != nil {
		srv.SetBundle(b)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, &countingListener{Listener: ln, n: &n}); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return socket, &n
}

func TestResolveEnforce(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	write := func(json string) { _ = os.WriteFile(cfgPath, []byte(json), 0o600) }
	t.Setenv(envConfigPath, cfgPath)
	os.Unsetenv(envEnforce) // env genuinely absent → config decides

	// Default: no config field, no env → false (Phase-1 observe; never enforce by accident).
	write(`{"developer_did":"` + testDID + `"}`)
	if ResolveEnforce() {
		t.Error("default should be false (observe)")
	}

	// Config enables enforce mode.
	write(`{"developer_did":"` + testDID + `","enforce":true}`)
	if !ResolveEnforce() {
		t.Error("enforce:true in config should enable enforce mode")
	}

	// Env overrides config either way.
	t.Setenv(envEnforce, "false")
	if ResolveEnforce() {
		t.Error("env false must override config true")
	}
	write(`{"developer_did":"` + testDID + `"}`)
	t.Setenv(envEnforce, "1")
	if !ResolveEnforce() {
		t.Error("env 1 must override config absent/false")
	}
}

func TestBuildDecisionRequest(t *testing.T) {
	id := Identity{DeveloperDID: testDID}

	t.Run("bash carries the local-only command", func(t *testing.T) {
		ev := &HookEvent{
			SessionID:      "sess-1",
			PermissionMode: "default",
			ToolName:       "Bash",
			ToolInput:      []byte(`{"command":"rm -rf /tmp/x"}`),
		}
		req := buildDecisionRequest(id, ev)
		if req.SessionID != "sess-1" || req.DeveloperDID != testDID {
			t.Fatalf("identity not carried: %+v", req)
		}
		if req.EventType != client.EventToolCall {
			t.Errorf("event_type = %q, want ToolCall", req.EventType)
		}
		if req.Tool.Name != "Bash" || req.Tool.Kind != client.ToolShell {
			t.Errorf("tool = %+v, want Bash/shell", req.Tool)
		}
		if got := req.Attributes["command"]; got != "rm -rf /tmp/x" {
			t.Errorf("command attr = %q, want the shell command (local-only)", got)
		}
		if got := req.Attributes["permission_mode"]; got != "default" {
			t.Errorf("permission_mode = %q", got)
		}
	})

	t.Run("file tool carries path + operation, not a command", func(t *testing.T) {
		ev := &HookEvent{
			SessionID: "sess-2",
			ToolName:  "Write",
			ToolInput: []byte(`{"file_path":"/etc/passwd","content":"secret"}`),
		}
		req := buildDecisionRequest(id, ev)
		if req.Tool.Kind != client.ToolFile {
			t.Errorf("kind = %q, want file", req.Tool.Kind)
		}
		if got := req.Attributes["file_path"]; got != "/etc/passwd" {
			t.Errorf("file_path = %q", got)
		}
		if got := req.Attributes["file_operation"]; got != "write" {
			t.Errorf("file_operation = %q, want write", got)
		}
		if _, ok := req.Attributes["command"]; ok {
			t.Error("a file tool must not carry a command attribute")
		}
	})

	t.Run("mcp tool carries server + function", func(t *testing.T) {
		ev := &HookEvent{
			SessionID: "sess-3",
			ToolName:  "mcp__github__create_issue",
		}
		req := buildDecisionRequest(id, ev)
		if req.Tool.Kind != client.ToolMCP || req.Tool.MCPServer != "github" {
			t.Errorf("tool = %+v, want mcp/github", req.Tool)
		}
		if got := req.Attributes["mcp_function"]; got != "create_issue" {
			t.Errorf("mcp_function = %q", got)
		}
	})

	t.Run("no content field is ever set (INV-2)", func(t *testing.T) {
		ev := &HookEvent{SessionID: "s", ToolName: "Bash", ToolInput: []byte(`{"command":"ls"}`)}
		if req := buildDecisionRequest(id, ev); req.Content != nil {
			t.Error("Content must stay nil in E6-S1 (E6-S4 populates it, gated)")
		}
	})
}

func TestCapCommand_ByteBoundedRuneSafe(t *testing.T) {
	// A short command passes through unchanged.
	if got := capCommand("rm -rf /"); got != "rm -rf /" {
		t.Errorf("short command changed: %q", got)
	}
	// A long multibyte command must be bounded by BYTES (not runes) so the
	// marshaled request cannot overrun the server's byte read-limit (G_SEC LOW-1),
	// and must never split a rune (valid UTF-8 preserved).
	long := strings.Repeat("é", maxCommandLen) // 2 bytes/rune → ~2× the byte budget
	got := capCommand(long)
	if len(got) > maxCommandLen {
		t.Errorf("capCommand returned %d bytes, want <= %d (byte bound)", len(got), maxCommandLen)
	}
	if !utf8.ValidString(got) {
		t.Error("capCommand split a multibyte rune (invalid UTF-8)")
	}
}

func TestEnforceDecision_FailOpenWhenSidecarAbsent(t *testing.T) {
	// Point the client at a socket that does not exist → every fault fails open.
	cl := sidecar.NewClient(sidecar.ClientConfig{
		SocketPath: filepath.Join(t.TempDir(), "nope.sock"),
		Timeout:    50 * time.Millisecond,
	})
	ev := &HookEvent{SessionID: "s", ToolName: "Bash", ToolInput: []byte(`{"command":"rm -rf /"}`)}

	start := time.Now()
	dec := EnforceDecision(context.Background(), cl, Identity{DeveloperDID: testDID}, ev)
	elapsed := time.Since(start)

	if !dec.FailOpen {
		t.Error("absent sidecar must fail open")
	}
	if dec.Evaluation.Verdict != client.VerdictUnknown {
		t.Errorf("fail-open verdict = %q, want Unknown (not a real ALLOW/BLOCK)", dec.Evaluation.Verdict)
	}
	if dec.Evaluation.WouldBlock() {
		t.Error("fail-open decision must never report WouldBlock")
	}
	if elapsed > 2*time.Second {
		t.Errorf("fail-open took %v — must return promptly within the bound", elapsed)
	}
}

func TestEnforceDecision_LiveBlock(t *testing.T) {
	bundle := &sidecar.Bundle{
		Version:         "test-1",
		DefaultDecision: "allow",
		Rules: []sidecar.Rule{{
			ID: "no-rm-rf",
			Match: sidecar.RuleMatch{
				ToolKind:          "shell",
				AttributeContains: map[string]string{"command": "rm -rf"},
			},
			Decision: "block",
			Reason:   "destructive recursive delete",
			PolicyID: "test-policy",
		}},
	}
	socket, accepts := serveSidecar(t, bundle)
	cl := sidecar.NewClient(sidecar.ClientConfig{SocketPath: socket})

	// A dangerous command → the local policy returns BLOCK (obtained synchronously).
	danger := &HookEvent{SessionID: "s", ToolName: "Bash", ToolInput: []byte(`{"command":"rm -rf /tmp/x"}`)}
	dec := EnforceDecision(context.Background(), cl, Identity{DeveloperDID: testDID}, danger)
	if dec.FailOpen {
		t.Fatalf("live sidecar should not fail open: %+v", dec)
	}
	if dec.Evaluation.Verdict != client.VerdictBlock || !dec.Evaluation.WouldBlock() {
		t.Errorf("verdict = %q (would_block=%t), want BLOCK", dec.Evaluation.Verdict, dec.Evaluation.WouldBlock())
	}

	// A benign command → default allow.
	benign := &HookEvent{SessionID: "s", ToolName: "Bash", ToolInput: []byte(`{"command":"echo hi"}`)}
	if d := EnforceDecision(context.Background(), cl, Identity{DeveloperDID: testDID}, benign); d.Evaluation.WouldBlock() {
		t.Errorf("benign command should not block: %+v", d)
	}

	if atomic.LoadInt32(accepts) < 2 {
		t.Errorf("expected the sidecar to be dialed for each decision, got %d accepts", *accepts)
	}
}

// TestRunHook_EnforceGate is the AC-2/AC-4 guard: with enforce OFF the sidecar is
// NEVER dialed (observe path byte-unchanged); with enforce ON a PreToolUse dials
// it exactly once and logs the decision — and NOTHING is written to stdout in
// either mode (INV-3 holds verbatim for E6-S1).
func TestRunHook_EnforceGate(t *testing.T) {
	socket, accepts := serveSidecar(t, nil) // cold-start server → fail-open ALLOW
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envSidecarSocket, socket)

	payload := `{"hook_event_name":"PreToolUse","session_id":"sess-1","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`

	run := func() string {
		var stderr bytes.Buffer
		logger := log.New(&stderr, "", 0)
		RunHook("PreToolUse", strings.NewReader(payload), logger)
		return stderr.String()
	}

	// Enforce OFF → the sidecar is not contacted; no enforce diagnostic emitted.
	t.Setenv(envEnforce, "0")
	if out := run(); strings.Contains(out, "enforce decision:") {
		t.Errorf("enforce-off must not run the gate; stderr=%q", out)
	}
	if n := atomic.LoadInt32(accepts); n != 0 {
		t.Fatalf("enforce-off dialed the sidecar %d time(s) — observe path must not touch it", n)
	}

	// Enforce ON → the PreToolUse hook synchronously dials the sidecar once.
	t.Setenv(envEnforce, "1")
	out := run()
	if !strings.Contains(out, "enforce decision:") {
		t.Errorf("enforce-on must obtain + log a decision; stderr=%q", out)
	}
	if n := atomic.LoadInt32(accepts); n != 1 {
		t.Errorf("enforce-on should dial the sidecar exactly once, got %d", n)
	}
}

// TestRunHook_EnforceOnlyPreToolUse guards AC-6: even in enforce mode, a
// non-PreToolUse hook never dials the sidecar (the gate is a pre-execution concept).
func TestRunHook_EnforceOnlyPreToolUse(t *testing.T) {
	socket, accepts := serveSidecar(t, nil)
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envSidecarSocket, socket)
	t.Setenv(envEnforce, "1")

	// PostToolUse in enforce mode must still only observe.
	payload := `{"hook_event_name":"PostToolUse","session_id":"sess-1","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`
	logger := log.New(&bytes.Buffer{}, "", 0)
	RunHook("PostToolUse", strings.NewReader(payload), logger)
	if n := atomic.LoadInt32(accepts); n != 0 {
		t.Errorf("PostToolUse must not dial the sidecar even in enforce mode, got %d", n)
	}
}
