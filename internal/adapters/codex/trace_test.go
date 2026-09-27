package codex

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// TestRunHookTracesRawStdinAndNeverLeaksCredentials is the codex half of the
// phase-2 end-to-end test also covered for claude-code: a gated PreToolUse
// call whose tool_input carries a secret-shaped (code-derived, never
// committed) literal is traced in full -- hook.in records the RAW payload,
// including that literal -- and no trace record anywhere carries the fake
// bearer/API credential this test's own fakecore harness authenticates with.
func TestRunHookTracesRawStdinAndNeverLeaksCredentials(t *testing.T) {
	isolateEnforce(t)
	serveVerdict(t, `{"verdict":"allow"}`)

	traceDir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: traceDir})
	defer restore()

	secretLike := "AKIA" + strings.Repeat("Q", 16) // AWS-access-key SHAPED, derived here, never real
	payload := `{"hook_event_name":"PreToolUse","session_id":"sess-trace-1","cwd":"/tmp",` +
		`"tool_name":"Write","tool_input":{"file_path":"/tmp/x.txt","content":"` + secretLike + `"}}`

	var out bytes.Buffer
	RunHook("PreToolUse", strings.NewReader(payload), &out, log.New(&bytes.Buffer{}, "", 0))

	recs, skipped, err := trace.Read(traceDir, nil)
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("trace.Read skipped %d corrupt line(s)", skipped)
	}
	if len(recs) == 0 {
		t.Fatal("expected at least one trace record")
	}

	forbidden := []string{fakecore.APIKey(), fakecore.WorkloadPrivateKey()}

	var sawHookIn, hookInHasSecret bool
	for _, r := range recs {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal record: %v", err)
		}
		s := string(raw)
		for _, f := range forbidden {
			if f != "" && strings.Contains(s, f) {
				t.Fatalf("trace record leaked a credential (%q): %s", f, s)
			}
		}
		if r.Stage == trace.StageHookIn {
			sawHookIn = true
			if r.Provider != provider {
				t.Errorf("hook.in Provider = %q, want %q", r.Provider, provider)
			}
			if strings.Contains(s, secretLike) {
				hookInHasSecret = true
			}
		}
	}
	if !sawHookIn {
		t.Fatal("expected a hook.in record")
	}
	if !hookInHasSecret {
		t.Fatal("hook.in should carry the raw (pre-redaction) stdin, including the secret-shaped literal")
	}
}

// TestRunHookTracesHookOut confirms a hook.out record is written with the
// hook's own stdout and a duration, alongside hook.in, for an ordinary
// observe-only (non-gated) hook.
func TestRunHookTracesHookOut(t *testing.T) {
	isolateEnforce(t)

	traceDir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: traceDir})
	defer restore()

	payload := `{"hook_event_name":"PostToolUse","session_id":"sess-trace-2","cwd":"/tmp",` +
		`"tool_name":"Bash","tool_input":{"command":"echo hi"}}`
	var out bytes.Buffer
	RunHook("PostToolUse", strings.NewReader(payload), &out, log.New(&bytes.Buffer{}, "", 0))

	recs, _, err := trace.Read(traceDir, nil)
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	var sawIn, sawOut bool
	for _, r := range recs {
		switch r.Stage {
		case trace.StageHookIn:
			sawIn = true
			if r.SessionID != "sess-trace-2" {
				t.Errorf("hook.in SessionID = %q, want sess-trace-2", r.SessionID)
			}
		case trace.StageHookOut:
			sawOut = true
			if r.SessionID != "sess-trace-2" {
				t.Errorf("hook.out SessionID = %q, want sess-trace-2", r.SessionID)
			}
			if r.DurMS < 0 {
				t.Errorf("hook.out DurMS = %v, want >= 0", r.DurMS)
			}
		}
	}
	if !sawIn || !sawOut {
		t.Fatalf("expected both hook.in and hook.out records, got in=%v out=%v", sawIn, sawOut)
	}
}
