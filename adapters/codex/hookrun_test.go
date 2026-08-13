package codex

import (
	"bytes"
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
)

// setHookEnv isolates RunHook from the real machine: temp spool + nonexistent
// dev.json, identity via env, and EVERY default-real-path sink pinned to the
// temp dir (G_SEC SL7-A F3): hermeticity must be structural, never dependent
// on a mock's verdict values keeping a sink un-written (the INC-SL7A-DEVJSON
// failure class). Returns the spool dir.
func setHookEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	t.Setenv("OPENBOX_AGENT_DID", testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", spool)
	t.Setenv("OPENBOX_CONFIG", filepath.Join(dir, "none.json"))
	t.Setenv("OPENBOX_ADVISORY_FILE", filepath.Join(dir, "advisories.jsonl"))
	// SL7-B sinks, pinned now so the enforce leg inherits a hermetic helper.
	t.Setenv("OPENBOX_FINDINGS_CURSOR", filepath.Join(dir, "findings.cursor"))
	t.Setenv("OPENBOX_ENFORCEMENT_FILE", filepath.Join(dir, "enforcements.jsonl"))
	return spool
}

func runHook(t *testing.T, sub, payload string) (stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	logger := log.New(&errb, "openbox hook: ", 0)
	RunHook(sub, strings.NewReader(payload), &out, logger)
	return out.String(), errb.String()
}

// TestRunHook_ObserveOnlyContract (AC-3/AC-7 in-process): a PreToolUse with
// content in tool_input writes NOTHING to stdout, spools one ToolCall, and the
// content never reaches the spool.
func TestRunHook_ObserveOnlyContract(t *testing.T) {
	spool := setHookEnv(t)
	secret := "TOP-SECRET-COMMAND-do-not-egress"
	payload := `{"hook_event_name":"PreToolUse","session_id":"th-xyz","cwd":"/r","model":"gpt-5.3-codex",` +
		`"permission_mode":"default","turn_id":"t1","tool_name":"Bash","tool_use_id":"call-1",` +
		`"tool_input":{"command":"` + secret + `"},"transcript_path":null}`

	stdout, _ := runHook(t, "PreToolUse", payload)
	if stdout != "" {
		t.Fatalf("stdout must be empty (Codex parses hook stdout as output JSON), got %q", stdout)
	}

	entries, err := os.ReadDir(spool)
	if err != nil {
		t.Fatalf("read spool dir: %v", err)
	}
	var file string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			file = e.Name()
		}
	}
	if file == "" {
		t.Fatalf("no spool file written, entries=%v", entries)
	}
	raw, _ := os.ReadFile(filepath.Join(spool, file))
	if !strings.Contains(string(raw), "ToolCall") {
		t.Errorf("spooled event should be a ToolCall: %s", raw)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("command content leaked into the spool: %s", raw)
	}
}

// Misuse (bad hook name, empty payload, missing identity) is safe: no stdout,
// no panic, nothing spooled.
func TestRunHook_MisuseIsSafe(t *testing.T) {
	spool := setHookEnv(t)
	for _, tc := range []struct{ sub, payload string }{
		{"", ""},
		{"Stop", `{"session_id":"th-1"}`}, // recognized Codex event, not wired
		{"PreToolUse", ""},                // empty payload
		{"PreToolUse", "{not json"},
	} {
		if stdout, _ := runHook(t, tc.sub, tc.payload); stdout != "" {
			t.Errorf("sub=%q wrote stdout: %q", tc.sub, stdout)
		}
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		t.Errorf("misuse spooled something: %v", entries)
	}

	// Missing identity: event dropped fail-open.
	t.Setenv("OPENBOX_AGENT_DID", "")
	if stdout, stderr := runHook(t, "PreToolUse", `{"session_id":"th-1","tool_name":"Bash"}`); stdout != "" || !strings.Contains(stderr, "no identity") {
		t.Errorf("missing identity: stdout=%q stderr=%q", stdout, stderr)
	}
}

// TestRunHook_SessionEndFlushesSpool (AC-6): SessionEnd drains the session's
// spooled events through the real signed client to a loopback core — with no
// content on the wire — and stdout stays empty throughout.
func TestRunHook_SessionEndFlushesSpool(t *testing.T) {
	setHookEnv(t)
	var mu bytesBuffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.append(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"verdict":"allow"}`))
	}))
	defer srv.Close()
	t.Setenv("OPENBOX_BASE_URL", srv.URL) // loopback http allowed (INV-1 guard)
	t.Setenv("OPENBOX_API_KEY", "obx_test_key")
	t.Setenv("OPENBOX_ED25519_SEED", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	// Content capture OFF, stated rather than inherited, because this case is
	// about the FLUSH path carrying no content — not about the content posture.
	//
	// It used to get that for free: with the gate narrowed to escalating
	// nothing by default, the spooled metadata-only copy was the only thing
	// that ever reached the wire. ADR-0017 evaluates every gated call inline,
	// and an escalation DOES attach content when capture is on (E7), so leaving
	// the default here would assert the absence of something the design now
	// deliberately sends. That posture belongs to its own tests.
	t.Setenv(devconfig.EnvContentCapture, "0")

	secret := "FLUSH-SECRET-COMMAND"
	pre := `{"hook_event_name":"PreToolUse","session_id":"th-flush","cwd":"/r","tool_name":"Bash",` +
		`"tool_use_id":"call-1","tool_input":{"command":"` + secret + `"}}`
	end := `{"hook_event_name":"SessionEnd","session_id":"th-flush","cwd":"/r","reason":"other","transcript_path":null}`

	if stdout, _ := runHook(t, "PreToolUse", pre); stdout != "" {
		t.Fatalf("PreToolUse stdout: %q", stdout)
	}
	if stdout, _ := runHook(t, "SessionEnd", end); stdout != "" {
		t.Fatalf("SessionEnd stdout: %q", stdout)
	}

	bodies := mu.get()
	if len(bodies) != 2 { // ToolCall + SessionEnded
		t.Fatalf("expected 2 delivered events, got %d", len(bodies))
	}
	for _, b := range bodies {
		if strings.Contains(string(b), secret) {
			t.Fatalf("content leaked to the wire: %s", b)
		}
	}
}

// Offline flush is fail-open: SessionEnd logs and leaves the events spooled for
// a later `flush` — never an error surface, never stdout.
func TestRunHook_OfflineFlushFailsOpen(t *testing.T) {
	spool := setHookEnv(t)
	// No API key/seed configured → flush is skipped, spool retained.
	pre := `{"hook_event_name":"PreToolUse","session_id":"th-off","tool_name":"Bash","tool_use_id":"c1"}`
	end := `{"hook_event_name":"SessionEnd","session_id":"th-off","reason":"other"}`
	runHook(t, "PreToolUse", pre)
	stdout, stderr := runHook(t, "SessionEnd", end)
	if stdout != "" {
		t.Fatalf("stdout must stay empty on a failed flush: %q", stdout)
	}
	if !strings.Contains(stderr, "flush skipped") {
		t.Errorf("expected fail-open flush diagnostic, got %q", stderr)
	}
	// Events remain spooled (both the ToolCall and the SessionEnded line).
	entries, _ := os.ReadDir(spool)
	found := false
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			found = true
		}
	}
	if !found {
		t.Errorf("spool should retain events after a failed flush: %v", entries)
	}
}

// bytesBuffer is a tiny mutex-guarded [][]byte for the httptest handler.
type bytesBuffer struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (b *bytesBuffer) append(raw []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bodies = append(b.bodies, raw)
}

func (b *bytesBuffer) get() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([][]byte, len(b.bodies))
	copy(out, b.bodies)
	return out
}
