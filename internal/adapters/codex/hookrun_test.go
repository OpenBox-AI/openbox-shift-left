package codex

import (
	"bytes"
	"log"

	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig/devconfigtest"
	"github.com/openbox-ai/openbox-shift-left/internal/client/workloadauth"
)

func setHookEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	t.Setenv(devconfig.EnvHome, dir)
	bindForTest(t, "codex")
	t.Setenv(devconfig.EnvAgentID, testAgentID)
	t.Setenv("OPENBOX_SPOOL_DIR", spool)
	t.Setenv("OPENBOX_CONFIG", filepath.Join(dir, "none.json"))
	t.Setenv("OPENBOX_ADVISORY_FILE", filepath.Join(dir, "advisories.jsonl"))
	t.Setenv("OPENBOX_FINDINGS_CURSOR", filepath.Join(dir, "findings.cursor"))
	t.Setenv("OPENBOX_ENFORCEMENT_FILE", filepath.Join(dir, "enforcements.jsonl"))
	// This helper's own callers are a mix of gated- and never-gated-hook
	// tests, and most of them (SessionStart/PostToolUse/SessionEnd/finops/
	// turn tests) inspect the LOCAL spool file afterward, which only works
	// with no reachable core (nothing flushes away what they want to
	// inspect). DefaultHaltDir falls back to the real OS $HOME
	// (os.UserConfigDir), not devconfig.EnvHome above: with no reachable
	// control plane, delivery is always fail-closed now
	// (HaltOnDeliveryFailure), so a caller here that DOES exercise a gated
	// hook would otherwise latch into the process-wide sentinel HOME
	// (testmain_test.go).
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	return spool
}

func runHook(t *testing.T, sub, payload string) (stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	logger := log.New(&errb, "openbox hook: ", 0)
	RunHook(sub, strings.NewReader(payload), &out, logger)
	return out.String(), errb.String()
}

// TestRunHook_ObserveOnlyContract (AC-3/AC-7 in-process): a PreToolUse spools
// one ToolCall and the content never reaches the spool. PreToolUse is gated
// unconditionally now (ResolveEnforce always reports true); with no reachable
// control plane the gate itself denies (delivery is always fail-closed), but
// the escalation was never attempted (no client configured), so the gate's
// own SpoolObserve still appends this call's observe copy to the local spool
// as its first delivery attempt -- what this test actually exercises.
func TestRunHook_ObserveOnlyContract(t *testing.T) {
	spool := setHookEnv(t)
	secret := "TOP-SECRET-COMMAND-do-not-egress"
	payload := `{"hook_event_name":"PreToolUse","session_id":"th-xyz","cwd":"/r","model":"gpt-5.3-codex",` +
		`"permission_mode":"default","turn_id":"t1","tool_name":"Bash","tool_use_id":"call-1",` +
		`"tool_input":{"command":"` + secret + `"},"transcript_path":null}`

	stdout, _ := runHook(t, "PreToolUse", payload)
	d, _, _ := parsePreToolUse(t, []byte(stdout))
	if d != codexDecisionDeny {
		t.Fatalf("no reachable control plane must deny (delivery is always fail-closed); permissionDecision = %q, stdout=%q", d, stdout)
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

// TestRunHook_MisuseIsSafe misuse (bad hook name, empty payload, missing
// identity) is safe: no stdout, no panic, nothing spooled.
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

	t.Setenv(devconfig.EnvAgentID, "")
	if stdout, stderr := runHook(t, "PreToolUse", `{"session_id":"th-1","tool_name":"Bash"}`); stdout != "" || !strings.Contains(stderr, "no identity") {
		t.Errorf("missing identity: stdout=%q stderr=%q", stdout, stderr)
	}
}

// TestLegacyStoreHookSendsNothing a stored developer_did means the store
// predates v3 entirely: ResolveIdentity refuses with ErrLegacyStore, the
// mapper never even sees the event, and the hook stays exactly as silent as
// it is for a genuinely unconfigured machine -- an unmapped event, an empty
// spool, no request.
func TestLegacyStoreHookSendsNothing(t *testing.T) {
	spool := setHookEnv(t)
	if err := devconfigtest.SetLegacyDID(DefaultConfigPath(), "did:aip:legacy-store"); err != nil {
		t.Fatal(err)
	}

	stdout, stderr := runHook(t, "PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"th-legacy","cwd":"/r","tool_name":"Bash","tool_use_id":"c1"}`)
	if stdout != "" {
		t.Fatalf("stdout must stay empty, got %q", stdout)
	}
	if !strings.Contains(stderr, "no identity") {
		t.Errorf("expected a no-identity drop notice, got %q", stderr)
	}
	entries, _ := os.ReadDir(spool)
	if len(entries) != 0 {
		t.Errorf("a legacy store's event reached the spool: %v", entries)
	}
}

// TestRunHook_SessionEndFlushesSpool (AC-6): SessionEnd drains the session's
// spooled events through the real signed client to a loopback core; with no
// content on the wire; and stdout stays empty throughout.
func TestRunHook_SessionEndFlushesSpool(t *testing.T) {
	setHookEnv(t)
	srv := fakecore.New(t, fakecore.Script{})
	t.Setenv("OPENBOX_BASE_URL", srv.URL())
	t.Setenv(devconfig.EnvAPIKeyDirect, fakecore.APIKey())
	workloadKey, err := workloadauth.NormalizePrivateKey(fakecore.WorkloadPrivateKey())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(devconfig.EnvWorkloadPrivateKey, workloadKey)
	// That decision evaluates every gated call inline, and an escalation does
	// attach content when capture is on (E7), so leaving the default here would
	// assert the absence of something the design now deliberately sends.
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

	inbox := srv.Inbox()
	if len(inbox) != 2 { // ToolCall + SessionEnded
		t.Fatalf("expected 2 delivered events, got %d", len(inbox))
	}
	for _, r := range inbox {
		if strings.Contains(string(r.Raw), secret) {
			t.Fatalf("content leaked to the wire: %s", r.Raw)
		}
	}
}

// TestRunHook_OfflineFlushFailsOpen offline flush is fail-open: SessionEnd
// logs and leaves the events spooled for a later `flush`; never an error
// surface, never stdout.
func TestRunHook_OfflineFlushFailsOpen(t *testing.T) {
	spool := setHookEnv(t)
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
