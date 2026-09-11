package claudecode

import (
	"context"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// Both fixtures are assembled from fragments on purpose: this repo's own
// PreToolUse hook rewrites a secret-shaped literal in a file it writes, so a
// contiguous one would not survive on disk to be tested against.
func rotatedKeys() (oldKey, newKey string) {
	return "sk-ant-" + "api03-" + strings.Repeat("A1b2C3d4", 5),
		"sk-ant-" + "api03-" + strings.Repeat("Z9y8X7w6", 5)
}

// An Edit carries the text it replaces in old_string, and the local decider
// never sees it: fileText() reads content/new_string only, so buildDecisionRequest
// puts just that one field on the request. The file arm then rebuilds the gated
// copy with RedactToolInput, which preserves every other field byte-for-byte.
//
// That matters because the two copies are not both stored: EnforceGate.Run
// spools the fully-scanned observe copy only when the escalation did NOT
// deliver, so on a delivering /evaluate the rebuilt copy is the only row the
// control plane keeps. A live credential in old_string therefore egressed
// unscanned while the audit line recorded redacted:true -- and the arm engages
// whenever the redactor fires on new_string, which an ordinary key rotation
// does.
func TestEnforceCopy_ScansFieldsTheDeciderNeverSaw(t *testing.T) {
	oldKey, newKey := rotatedKeys()

	ev := &HookEvent{
		SessionID: "s1", ToolName: "Edit",
		ToolInput: []byte(`{"file_path":"/tmp/.env",` +
			`"old_string":"ANTHROPIC_API_KEY=` + oldKey + `",` +
			`"new_string":"ANTHROPIC_API_KEY=` + newKey + `"}`),
	}

	redactor := decision.NewRedactor()
	m := testMapper()
	m.RedactContent = func(s string) string { return hookflow.RedactText(redactor, s) }

	// Exactly what the gate does: the local decider runs first, and its result
	// is what DevEvent rebuilds the gated copy from.
	dec := redactor.Decide(context.Background(), buildDecisionRequest(Identity{DeveloperDID: testDID}, ev, true))
	if dec.RedactedContent == nil || dec.RedactedContent.FileText == "" {
		t.Fatal("the fixture must engage the file arm: new_string has to redact")
	}

	got, ok := enforceTarget{id: Identity{DeveloperDID: testDID}, mapper: m, ev: ev}.DevEvent(dec.RedactedContent)
	if !ok || got.Content == nil {
		t.Fatal("a gated Edit must carry its body for evaluation (E7)")
	}

	if strings.Contains(got.Content.ToolInput, oldKey) {
		t.Errorf("old_string egressed unscanned; redaction must precede attachment for every\n"+
			"field of the rebuilt copy, not only the one the decider was handed: %q", got.Content.ToolInput)
	}
	if strings.Contains(got.Content.ToolInput, newKey) {
		t.Errorf("new_string egressed raw: %q", got.Content.ToolInput)
	}
	// E8 survives the added pass: the body is still the redacted one, and the
	// rebuild's structural fields are still there.
	if !strings.Contains(got.Content.ToolInput, "OPENBOX_REDACTED") {
		t.Errorf("no redaction placeholder in the attached body: %q", got.Content.ToolInput)
	}
	if !strings.Contains(got.Content.ToolInput, "/tmp/.env") {
		t.Errorf("file_path lost in the rebuild: %q", got.Content.ToolInput)
	}
}

// The counterweight: the copy written BACK to Claude Code replays into the
// developer's actual file, and an Edit only applies when old_string still
// matches the bytes on disk. ApplyInputRedaction must therefore keep it
// verbatim -- scanning the egress copy must not leak into the write-back.
func TestInputRedaction_LeavesOldStringIntactForTheDiskRewrite(t *testing.T) {
	oldKey, newKey := rotatedKeys()

	ev := &HookEvent{
		SessionID: "s1", ToolName: "Edit",
		ToolInput: []byte(`{"file_path":"/tmp/.env",` +
			`"old_string":"ANTHROPIC_API_KEY=` + oldKey + `",` +
			`"new_string":"ANTHROPIC_API_KEY=` + newKey + `"}`),
	}

	redactor := decision.NewRedactor()
	dec := redactor.Decide(context.Background(), buildDecisionRequest(Identity{DeveloperDID: testDID}, ev, true))

	updated := hookflow.ApplyInputRedaction(dec, true, ev.ToolInput, contentFieldKeys)
	if len(updated) == 0 {
		t.Fatal("a detected secret in new_string must produce an updatedInput")
	}
	if !strings.Contains(string(updated), oldKey) {
		t.Errorf("old_string was rewritten; the Edit would no longer match the file on disk: %s", updated)
	}
	if strings.Contains(string(updated), newKey) {
		t.Errorf("new_string was not redacted in the disk rewrite: %s", updated)
	}
}
