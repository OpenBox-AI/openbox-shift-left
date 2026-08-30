package claudecode

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

func mkReasons(cats ...string) []client.GuardrailReason {
	out := make([]client.GuardrailReason, 0, len(cats))
	for _, c := range cats {
		out = append(out, client.GuardrailReason{Type: c})
	}
	return out
}

func findingsEnv(t *testing.T, on bool) (string, string) {
	t.Helper()
	dir := t.TempDir()
	adv := filepath.Join(dir, "advisories.jsonl")
	cur := filepath.Join(dir, "findings.cursor")
	t.Setenv("OPENBOX_ADVISORY_FILE", adv)
	t.Setenv(devconfig.EnvFindingsCursor, cur)
	if on {
		t.Setenv(devconfig.EnvFindings, "1")
	} else {
		t.Setenv(devconfig.EnvFindings, "0")
	}
	return adv, cur
}

func seedAdvisories(t *testing.T, path string, recs ...hookflow.AdvisoryRecord) {
	t.Helper()
	var buf bytes.Buffer
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal advisory: %v", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
}

func mkReasonsFull(typ, field, reason string) []client.GuardrailReason {
	return []client.GuardrailReason{{Type: typ, Field: field, Reason: reason}}
}

func nopLogger() *log.Logger { return log.New(&bytes.Buffer{}, "", 0) }

func TestRunHook_FindingsSurfacedOnPostToolUseAndPrompt(t *testing.T) {
	adv, _ := findingsEnv(t, true)
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	seedAdvisories(t, adv,
		hookflow.AdvisoryRecord{Verdict: "ALLOW", GuardrailReasons: mkReasonsFull("secret", "api_key", "leaked AKIAsecretVALUE123")},
		hookflow.AdvisoryRecord{Verdict: "ALLOW", DriftDetected: true, DriftViolations: 1},
	)

	for _, hook := range []string{"PostToolUse", "UserPromptSubmit"} {
		t.Setenv(envFindingsCursor, filepath.Join(t.TempDir(), "cursor"))
		var out bytes.Buffer
		payload := `{"hook_event_name":"` + hook + `","session_id":"sess-1","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"},"prompt":"hello"}`
		RunHook(hook, strings.NewReader(payload), &out, nopLogger())
		s := out.String()
		if !strings.Contains(s, "OpenBox governance") {
			t.Errorf("%s with findings ON should surface a summary, got %q", hook, s)
		}
		for _, banned := range []string{"AKIAsecretVALUE123", "leaked", "api_key"} {
			if strings.Contains(s, banned) {
				t.Errorf("%s leaked content %q: %s", hook, banned, s)
			}
		}
		if !strings.Contains(s, "guardrails [secret]") || !strings.Contains(s, "goal-drift") {
			t.Errorf("%s summary missing category/drift: %s", hook, s)
		}
		assertFindingsShape(t, s, HookName(hook))
	}
}

func TestRunHook_FindingsOffIsByteIdentical(t *testing.T) {
	adv, _ := findingsEnv(t, false) // findings OFF
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	seedAdvisories(t, adv, hookflow.AdvisoryRecord{Verdict: "BLOCK", WouldBlock: true, RiskScore: 0.9})

	for _, hook := range []string{"PostToolUse", "UserPromptSubmit"} {
		var out bytes.Buffer
		payload := `{"hook_event_name":"` + hook + `","session_id":"sess-1","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"},"prompt":"hi"}`
		RunHook(hook, strings.NewReader(payload), &out, nopLogger())
		if out.Len() != 0 {
			t.Errorf("findings OFF: %s must write nothing, got %q", hook, out.String())
		}
	}
}

// TestRunHook_FindingsNotSurfacedOnOtherHooks: only PostToolUse +
// UserPromptSubmit surface; PreToolUse/SessionStart/SessionEnd never do (they
// own other stdout).
func TestRunHook_FindingsNotSurfacedOnOtherHooks(t *testing.T) {
	adv, _ := findingsEnv(t, true)
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	seedAdvisories(t, adv, hookflow.AdvisoryRecord{Verdict: "BLOCK", WouldBlock: true})

	var out bytes.Buffer
	payload := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Read","tool_input":{"file_path":"/x"}}`
	RunHook("PreToolUse", strings.NewReader(payload), &out, nopLogger())
	if strings.Contains(out.String(), "OpenBox governance") {
		t.Errorf("PreToolUse must not surface findings, got %q", out.String())
	}
}

func assertFindingsShape(t *testing.T, blob string, hook HookName) {
	t.Helper()
	blob = strings.TrimSpace(blob)
	var m map[string]any
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		t.Fatalf("findings output is not one JSON object: %v (%q)", err, blob)
	}
	if _, ok := m["decision"]; ok {
		t.Error("INV-3: findings output must not carry a `decision`")
	}
	hso, ok := m["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatal("missing hookSpecificOutput")
	}
	if _, ok := hso["permissionDecision"]; ok {
		t.Error("INV-3: findings output must not carry a `permissionDecision`")
	}
	if hso["hookEventName"] != string(hook) {
		t.Errorf("hookEventName = %v, want %s", hso["hookEventName"], hook)
	}
	if _, ok := hso["additionalContext"].(string); !ok {
		t.Error("missing additionalContext")
	}
	if _, ok := m["systemMessage"].(string); !ok {
		t.Error("missing systemMessage")
	}
}
