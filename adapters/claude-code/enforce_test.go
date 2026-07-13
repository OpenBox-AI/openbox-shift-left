package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
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
		var stderr, stdout bytes.Buffer
		logger := log.New(&stderr, "", 0)
		RunHook("PreToolUse", strings.NewReader(payload), &stdout, logger)
		// A cold-start (bundle-less) sidecar fails open → VerdictUnknown → the apply
		// emits NOTHING (tighten-only): stdout must stay empty in every case here.
		if stdout.Len() != 0 {
			t.Errorf("fail-open/allow decision must not write to stdout; stdout=%q", stdout.String())
		}
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
	RunHook("PostToolUse", strings.NewReader(payload), &bytes.Buffer{}, logger)
	if n := atomic.LoadInt32(accepts); n != 0 {
		t.Errorf("PostToolUse must not dial the sidecar even in enforce mode, got %d", n)
	}
}

// ── E6-S2: apply(verdict) tests ──────────────────────────────────────────────

// parsePermissionDecision decodes a PreToolUse hook stdout blob and returns the
// permissionDecision + reason (empty decision when nothing was written).
func parsePermissionDecision(t *testing.T, out []byte) (decision, reason string) {
	t.Helper()
	if len(bytes.TrimSpace(out)) == 0 {
		return "", ""
	}
	var got preToolUseOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("stdout is not valid PreToolUse JSON: %v (%q)", err, out)
	}
	if got.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Errorf("hookEventName = %q, want PreToolUse", got.HookSpecificOutput.HookEventName)
	}
	return got.HookSpecificOutput.PermissionDecision, got.HookSpecificOutput.PermissionDecisionReason
}

// TestMapVerdict exercises the full SDK cascade port (OD-ENF-SCOPE):
// HALT/BLOCK/guardrail-fail → deny; REQUIRE_APPROVAL → ask; CONSTRAIN/ALLOW/
// UNKNOWN → proceed (no decision). Order matters: a guardrail failure denies
// regardless of the (non-HALT/BLOCK) verdict, matching verdict_handler.py:84-90.
func TestMapVerdict(t *testing.T) {
	guardFail := &client.GuardrailResult{Passed: false, Reasons: []client.GuardrailReason{{Type: "pii", Reason: "secret detail"}}}
	guardPass := &client.GuardrailResult{Passed: true}

	cases := []struct {
		name     string
		eval     client.Evaluation
		wantDec  string
		wantEmit bool
		// reasonNoLeak asserts the free text below is NOT present in the reason.
		reasonNoLeak string
		reasonHas    string
	}{
		{"halt denies", client.Evaluation{Verdict: client.VerdictHalt, Reason: "kill switch"}, ccDecisionDeny, true, "", "kill switch"},
		{"block denies with policy id", client.Evaluation{Verdict: client.VerdictBlock, Reason: "no rm -rf", PolicyID: "p-1"}, ccDecisionDeny, true, "", "p-1"},
		{"require_approval asks", client.Evaluation{Verdict: client.VerdictRequireApproval, Reason: "needs review"}, ccDecisionAsk, true, "", "needs review"},
		{"constrain proceeds", client.Evaluation{Verdict: client.VerdictConstrain, Reason: "scoped"}, "", false, "", ""},
		{"allow proceeds", client.Evaluation{Verdict: client.VerdictAllow}, "", false, "", ""},
		{"unknown (fail-open) proceeds", client.Evaluation{Verdict: client.VerdictUnknown}, "", false, "", ""},
		// Guardrail failure denies even when the verdict itself would proceed, and
		// the reason carries only the CATEGORY (pii), never the free text (INV-2).
		{"guardrail fail denies (category only)", client.Evaluation{Verdict: client.VerdictAllow, Guardrail: guardFail}, ccDecisionDeny, true, "secret detail", "pii"},
		{"guardrail pass does not deny", client.Evaluation{Verdict: client.VerdictAllow, Guardrail: guardPass}, "", false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dec, reason := mapVerdict(c.eval)
			if dec != c.wantDec {
				t.Errorf("decision = %q, want %q", dec, c.wantDec)
			}
			if (dec != "") != c.wantEmit {
				t.Errorf("emit = %t, want %t", dec != "", c.wantEmit)
			}
			if c.reasonHas != "" && !strings.Contains(reason, c.reasonHas) {
				t.Errorf("reason %q missing %q", reason, c.reasonHas)
			}
			if c.reasonNoLeak != "" && strings.Contains(reason, c.reasonNoLeak) {
				t.Errorf("reason %q leaked guardrail free text %q (INV-2)", reason, c.reasonNoLeak)
			}
		})
	}
}

func TestApplyDecision(t *testing.T) {
	t.Run("block writes a deny permissionDecision", func(t *testing.T) {
		var out bytes.Buffer
		dec := sidecar.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictBlock, Reason: "destructive"}}
		applied, emitted := applyDecision(&out, dec)
		if !emitted || applied != ccDecisionDeny {
			t.Fatalf("applied=%q emitted=%t, want deny/true", applied, emitted)
		}
		d, reason := parsePermissionDecision(t, out.Bytes())
		if d != "deny" {
			t.Errorf("permissionDecision = %q, want deny", d)
		}
		if !strings.Contains(reason, "destructive") {
			t.Errorf("reason = %q, want the policy reason surfaced", reason)
		}
	})

	t.Run("allow writes nothing (tighten-only)", func(t *testing.T) {
		var out bytes.Buffer
		dec := sidecar.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictAllow}}
		if applied, emitted := applyDecision(&out, dec); emitted || applied != "" {
			t.Errorf("allow must emit nothing, got applied=%q emitted=%t", applied, emitted)
		}
		if out.Len() != 0 {
			t.Errorf("allow wrote to stdout: %q", out.String())
		}
	})

	t.Run("fail-open (unknown) writes nothing", func(t *testing.T) {
		var out bytes.Buffer
		dec := sidecar.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictUnknown}, FailOpen: true}
		if _, emitted := applyDecision(&out, dec); emitted || out.Len() != 0 {
			t.Errorf("fail-open must not write a decision; stdout=%q", out.String())
		}
	})

	t.Run("nil stdout never panics and reports not-emitted", func(t *testing.T) {
		dec := sidecar.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictBlock}}
		if _, emitted := applyDecision(nil, dec); emitted {
			t.Error("a nil stdout must degrade to not-emitted (fail-open)")
		}
	})
}

// TestRunHook_EnforceApply_Block is the E6-S2 end-to-end guard: enforce ON + a
// live sidecar whose bundle blocks `rm -rf` → a PreToolUse hook writes a `deny`
// permissionDecision to stdout AND appends a content-free durable enforcement
// record. A benign command in the same session writes nothing (tighten-only) and
// records a proceed. INV-2: neither surface carries the shell command.
func TestRunHook_EnforceApply_Block(t *testing.T) {
	bundle := &sidecar.Bundle{
		Version:         "test-1",
		DefaultDecision: "allow",
		Rules: []sidecar.Rule{{
			ID:       "no-rm-rf",
			Match:    sidecar.RuleMatch{ToolKind: "shell", AttributeContains: map[string]string{"command": "rm -rf"}},
			Decision: "block",
			Reason:   "destructive recursive delete",
			PolicyID: "test-policy",
		}},
	}
	socket, _ := serveSidecar(t, bundle)
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envSidecarSocket, socket)
	t.Setenv(envEnforce, "1")
	enfFile := filepath.Join(t.TempDir(), "enforcements.jsonl")
	t.Setenv(envEnforcementFile, enfFile)

	run := func(payload string) string {
		var stdout bytes.Buffer
		logger := log.New(&bytes.Buffer{}, "", 0)
		RunHook("PreToolUse", strings.NewReader(payload), &stdout, logger)
		return stdout.String()
	}

	// Dangerous command → deny on stdout, with the policy reason (never the command).
	danger := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"rm -rf /tmp/x"}}`
	out := run(danger)
	d, reason := parsePermissionDecision(t, []byte(out))
	if d != "deny" {
		t.Fatalf("dangerous command: permissionDecision = %q, want deny (stdout=%q)", d, out)
	}
	if !strings.Contains(reason, "destructive recursive delete") || !strings.Contains(reason, "test-policy") {
		t.Errorf("reason = %q, want the policy reason + id", reason)
	}
	if strings.Contains(out, "rm -rf") {
		t.Errorf("stdout leaked the shell command (INV-2): %q", out)
	}

	// Benign command → nothing to stdout (governance only tightens).
	benign := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`
	if out := run(benign); strings.TrimSpace(out) != "" {
		t.Errorf("benign command must not write a decision; stdout=%q", out)
	}

	// The durable enforcement audit records both decisions, content-free.
	data, err := os.ReadFile(enfFile)
	if err != nil {
		t.Fatalf("enforcement sink not written: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 enforcement records, got %d: %q", len(lines), data)
	}
	if strings.Contains(string(data), "rm -rf") || strings.Contains(string(data), "echo hi") {
		t.Errorf("enforcement record leaked the shell command (INV-2): %q", data)
	}
	var first enforcementRecord
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("enforcement record is not valid JSON: %v", err)
	}
	if first.Verdict != string(client.VerdictBlock) || first.AppliedDecision != "deny" || !first.WouldBlock {
		t.Errorf("first record = %+v, want BLOCK/deny/would_block", first)
	}
	if first.PolicyID != "test-policy" || first.ToolKind != string(client.ToolShell) {
		t.Errorf("first record missing policy/tool metadata: %+v", first)
	}
}

// ── E6-S3: fail-open / fail-closed failure policy ────────────────────────────

// TestApplyFailurePolicy guards AC-2/AC-3/AC-4: the transform synthesizes a HALT
// ONLY on a fail-open decision under fail-closed; every other case is a no-op
// (fail-open proceeds; a real verdict is never overridden under either policy).
func TestApplyFailurePolicy(t *testing.T) {
	failOpenDec := sidecar.Decision{
		Evaluation: client.Evaluation{Verdict: client.VerdictUnknown, Reason: "sidecar unavailable"},
		FailOpen:   true,
		Source:     "fail-open-client",
	}

	t.Run("fail-open policy is a no-op even on an outage", func(t *testing.T) {
		got := applyFailurePolicy(failOpenDec, FailOpen)
		if got.Evaluation.Verdict != client.VerdictUnknown || got.Evaluation.WouldBlock() {
			t.Errorf("fail-open must leave the outage as UNKNOWN/proceed, got %+v", got.Evaluation)
		}
	})

	t.Run("fail-closed synthesizes HALT on an outage", func(t *testing.T) {
		got := applyFailurePolicy(failOpenDec, FailClosed)
		if got.Evaluation.Verdict != client.VerdictHalt || !got.Evaluation.WouldBlock() {
			t.Errorf("fail-closed must synthesize a blocking HALT, got %+v", got.Evaluation)
		}
		if !strings.Contains(got.Evaluation.Reason, "fail-closed") {
			t.Errorf("reason should mark fail-closed, got %q", got.Evaluation.Reason)
		}
		if !strings.Contains(got.Evaluation.Reason, "sidecar unavailable") {
			t.Errorf("reason should carry the content-free cause, got %q", got.Evaluation.Reason)
		}
		// The audit signature of a fail-closed deny: a HALT that is ALSO FailOpen
		// (a real HALT never carries FailOpen==true).
		if !got.FailOpen {
			t.Error("fail-closed synthetic HALT must retain FailOpen==true for the audit")
		}
	})

	// A REAL verdict from a reachable sidecar is never overridden — the policy
	// governs the evaluation-unavailable case only.
	for _, v := range []client.Verdict{client.VerdictAllow, client.VerdictConstrain, client.VerdictBlock} {
		t.Run("real "+string(v)+" untouched under fail-closed", func(t *testing.T) {
			real := sidecar.Decision{Evaluation: client.Evaluation{Verdict: v}, FailOpen: false}
			if got := applyFailurePolicy(real, FailClosed); got.Evaluation.Verdict != v {
				t.Errorf("real %s verdict must pass through fail-closed unchanged, got %q", v, got.Evaluation.Verdict)
			}
		})
	}
}

// TestLogEnforceDecision_PolicyLegible makes a fail-closed deny legible in the
// stderr diagnostic: a synthesized HALT (would_block=true) that is ALSO fail_open
// would look contradictory without the policy label.
func TestLogEnforceDecision_PolicyLegible(t *testing.T) {
	if FailOpen.String() != "fail_open" || FailClosed.String() != "fail_closed" {
		t.Fatalf("policy String() = %q/%q", FailOpen, FailClosed)
	}
	var buf bytes.Buffer
	dec := applyFailurePolicy(sidecar.Decision{
		Evaluation: client.Evaluation{Verdict: client.VerdictUnknown, Reason: "sidecar unavailable"},
		FailOpen:   true,
	}, FailClosed)
	logEnforceDecision(log.New(&buf, "", 0), &HookEvent{ToolName: "Bash"}, dec, FailClosed)
	out := buf.String()
	if !strings.Contains(out, "policy=fail_closed") || !strings.Contains(out, "fail_open=true") {
		t.Errorf("diagnostic should mark the fail-closed policy + fail_open cause; got %q", out)
	}
}

// TestRunHook_EnforceFailClosed is the AC-4 end-to-end guard: with enforce ON and
// fail_closed ON, an UNREACHABLE sidecar → a `deny` on stdout (a fail-closed deny),
// while a reachable sidecar's real ALLOW still PROCEEDS (the policy governs outages
// only). AC-7: enforce-OFF is byte-identical even under fail_closed=1. INV-2: the
// fail-closed reason carries no tool content.
func TestRunHook_EnforceFailClosed(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envEnforcementFile, filepath.Join(t.TempDir(), "enf.jsonl"))

	payload := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`
	run := func() string {
		var stdout bytes.Buffer
		RunHook("PreToolUse", strings.NewReader(payload), &stdout, log.New(&bytes.Buffer{}, "", 0))
		return stdout.String()
	}

	// enforce ON + fail_closed ON + NO sidecar reachable → deny.
	t.Setenv(envEnforce, "1")
	t.Setenv(envFailClosed, "1")
	t.Setenv(envSidecarSocket, filepath.Join(t.TempDir(), "absent.sock"))
	out := run()
	d, reason := parsePermissionDecision(t, []byte(out))
	if d != ccDecisionDeny {
		t.Fatalf("fail-closed + outage: permissionDecision = %q, want deny (stdout=%q)", d, out)
	}
	if !strings.Contains(reason, "fail-closed") {
		t.Errorf("fail-closed deny reason = %q, want it to explain the fail-closed outage", reason)
	}
	if strings.Contains(out, "echo hi") {
		t.Errorf("fail-closed reason leaked the shell command (INV-2): %q", out)
	}

	// A reachable sidecar with a benign command → real ALLOW → PROCEED even under
	// fail-closed (the policy does not touch a real allow verdict).
	socket, _ := serveSidecar(t, &sidecar.Bundle{Version: "t", DefaultDecision: "allow"})
	t.Setenv(envSidecarSocket, socket)
	if out := run(); strings.TrimSpace(out) != "" {
		t.Errorf("fail-closed must NOT block a real allow; stdout=%q", out)
	}

	// enforce OFF (still fail_closed=1) → byte-identical to observe: nothing to stdout.
	t.Setenv(envEnforce, "0")
	t.Setenv(envSidecarSocket, filepath.Join(t.TempDir(), "absent.sock"))
	if out := run(); strings.TrimSpace(out) != "" {
		t.Errorf("enforce-off must not deny even under fail_closed=1; stdout=%q", out)
	}
}

// TestRecordEnforcement_GuardrailCategoryOnly guards the durable-audit half of AC-6
// / INV-2 (G3 LOW-2, G_SEC LOW-1): a guardrail-failure decision records the CATEGORY
// type only — never the guardrail reason free text (which can describe detected
// content) or the field name.
func TestRecordEnforcement_GuardrailCategoryOnly(t *testing.T) {
	enfFile := filepath.Join(t.TempDir(), "enforcements.jsonl")
	t.Setenv(envEnforcementFile, enfFile)
	logger := log.New(&bytes.Buffer{}, "", 0)

	dec := sidecar.Decision{Source: "local-bundle", Evaluation: client.Evaluation{
		Verdict: client.VerdictAllow, // verdict alone would proceed…
		Guardrail: &client.GuardrailResult{Passed: false, Reasons: []client.GuardrailReason{
			{Type: "pii", Field: "ssn", Reason: "detected 123-45-6789 in the argument"},
		}},
	}}
	// …but a failed guardrail denies (mapVerdict) and the record captures it.
	applied, _ := applyDecision(&bytes.Buffer{}, dec)
	if applied != ccDecisionDeny {
		t.Fatalf("guardrail failure should deny, got %q", applied)
	}
	ev := &HookEvent{SessionID: "s", ToolName: "Write", ToolInput: []byte(`{"file_path":"/x"}`)}
	recordEnforcement(logger, ev, dec, applied)

	data, err := os.ReadFile(enfFile)
	if err != nil {
		t.Fatalf("enforcement sink not written: %v", err)
	}
	if !strings.Contains(string(data), "pii") {
		t.Errorf("record should carry the guardrail category; got %q", data)
	}
	for _, leak := range []string{"123-45-6789", "detected", "ssn"} {
		if strings.Contains(string(data), leak) {
			t.Errorf("record leaked guardrail free text/field %q (INV-2): %q", leak, data)
		}
	}
	var rec enforcementRecord
	_ = json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec)
	if len(rec.GuardrailCategories) != 1 || rec.GuardrailCategories[0] != "pii" {
		t.Errorf("guardrail_categories = %v, want [pii]", rec.GuardrailCategories)
	}
}
