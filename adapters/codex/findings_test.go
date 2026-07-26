package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// writeAdvisories writes JSONL advisory records to a temp file and returns its path.
func writeAdvisories(t *testing.T, recs ...advisoryRecord) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "advisories.jsonl")
	var buf bytes.Buffer
	for _, r := range recs {
		line, _ := json.Marshal(r)
		buf.Write(append(line, '\n'))
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSurfaceFindings_EmitsAdditionalContextAndSystemMessage(t *testing.T) {
	adv := writeAdvisories(t,
		advisoryRecord{Verdict: "BLOCK", WouldBlock: true, GuardrailReasons: []client.GuardrailReason{{Type: "pii"}, {Type: "secrets"}}},
		advisoryRecord{Verdict: "CONSTRAIN", DriftDetected: true, DriftViolations: 2, RiskScore: 0.9},
	)
	t.Setenv("OPENBOX_ADVISORY_FILE", adv)
	t.Setenv("OPENBOX_FINDINGS_CURSOR", filepath.Join(t.TempDir(), "cursor"))

	var out bytes.Buffer
	surfaceFindings(HookUserPromptSubmit, &out, nopLogger{})

	// Probe P2: Codex accepts additionalContext on UserPromptSubmit — full channel.
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("findings output not valid JSON: %v (%q)", err, out.String())
	}
	hso, _ := m["hookSpecificOutput"].(map[string]any)
	if hso == nil || hso["hookEventName"] != string(HookUserPromptSubmit) {
		t.Fatalf("expected hookSpecificOutput with hookEventName; got %v", m)
	}
	ac, _ := hso["additionalContext"].(string)
	sm, _ := m["systemMessage"].(string)
	if ac == "" || sm == "" {
		t.Fatalf("expected both additionalContext (→model) and systemMessage (→user); got ac=%q sm=%q", ac, sm)
	}
	// INV-3: no blocking/decision field is ever present.
	for _, banned := range []string{"permissionDecision", "decision", "continue", "stopReason"} {
		if _, ok := m[banned]; ok {
			t.Errorf("findings output must carry no decision field, found %q", banned)
		}
		if _, ok := hso[banned]; ok {
			t.Errorf("findings hookSpecificOutput must carry no decision field, found %q", banned)
		}
	}
	// INV-2: categories/counts only — never free text; the summary names categories.
	if !strings.Contains(ac, "pii") || !strings.Contains(ac, "would-block") {
		t.Errorf("summary should carry content-free categories/labels; got %q", ac)
	}
}

func TestSurfaceFindings_CursorAdvancesAtMostOnce(t *testing.T) {
	adv := writeAdvisories(t, advisoryRecord{Verdict: "BLOCK", WouldBlock: true})
	cursor := filepath.Join(t.TempDir(), "cursor")
	t.Setenv("OPENBOX_ADVISORY_FILE", adv)
	t.Setenv("OPENBOX_FINDINGS_CURSOR", cursor)

	var out1 bytes.Buffer
	surfaceFindings(HookPostToolUse, &out1, nopLogger{})
	if out1.Len() == 0 {
		t.Fatal("first surface should emit the finding")
	}
	// Second call with no new advisory bytes → nothing (cursor consumed the delta).
	var out2 bytes.Buffer
	surfaceFindings(HookPostToolUse, &out2, nopLogger{})
	if out2.Len() != 0 {
		t.Errorf("second surface with no new findings must write nothing; got %q", out2.String())
	}
}

func TestSurfaceFindings_NoSinkNoOp(t *testing.T) {
	t.Setenv("OPENBOX_ADVISORY_FILE", filepath.Join(t.TempDir(), "absent.jsonl"))
	t.Setenv("OPENBOX_FINDINGS_CURSOR", filepath.Join(t.TempDir(), "cursor"))
	var out bytes.Buffer
	surfaceFindings(HookPostToolUse, &out, nopLogger{}) // must not panic
	if out.Len() != 0 {
		t.Errorf("no advisory sink must surface nothing; got %q", out.String())
	}
}

func TestSummarizeFindings_ContentFree(t *testing.T) {
	// A guardrail reason with free text must contribute ONLY its category, never the
	// reason text (INV-2).
	adv, _ := json.Marshal(advisoryRecord{
		Verdict:          "BLOCK",
		WouldBlock:       true,
		GuardrailReasons: []client.GuardrailReason{{Type: "secrets", Reason: "found AKIA-style key in body"}},
	})
	sum := summarizeFindings(append(adv, '\n'))
	if strings.Contains(sum, "AKIA") || strings.Contains(sum, "found") {
		t.Errorf("summary leaked guardrail free text (INV-2): %q", sum)
	}
	if !strings.Contains(sum, "secrets") {
		t.Errorf("summary should carry the category: %q", sum)
	}
}

// nopLogger satisfies the findings logger interface.
type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}
