package gatewayemit

import (
	"context"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// TestNotElectedEmitYieldsCaptureOutcomeSkipped is phase 3's own contract on
// gatewayemit: an emitter that lost the election must not just emit nothing,
// it must leave a local trace record saying so, with the right reason --
// the same fact TestAnUnelectedLaneEmitsNothing checks against the spool.
func TestNotElectedEmitYieldsCaptureOutcomeSkipped(t *testing.T) {
	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	defer restore()

	em, _, _ := electionEmitter(t, func() bool { return false })
	em.Emit(context.Background(), capturedWithSession("sess-1"))

	recs, skipped, err := trace.Read(dir, nil)
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	var found *trace.Record
	for i := range recs {
		if recs[i].Stage == trace.StageCapture {
			found = &recs[i]
		}
	}
	if found == nil {
		t.Fatalf("no capture.outcome record found: %+v", recs)
	}
	if found.Outcome != "skipped" {
		t.Errorf("Outcome = %q, want %q", found.Outcome, "skipped")
	}
	if reason, _ := found.Detail["reason"].(string); reason != "not_elected" {
		t.Errorf("Detail[reason] = %v, want %q", found.Detail["reason"], "not_elected")
	}
}

// TestChatEmitYieldsCaptureOutcomeRecorded: a claude.ai chat completion has no
// tool session and no hook, so this relay's own capture.outcome record is the
// ONLY local trace of it ever happening; it must be recorded, not skipped.
func TestChatEmitYieldsCaptureOutcomeRecorded(t *testing.T) {
	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	defer restore()

	em, _ := newChatEmitter(t)
	em.Emit(context.Background(), chatCaptured(chatConvA, desktopUA))

	recs, skipped, err := trace.Read(dir, nil)
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	var found *trace.Record
	for i := range recs {
		if recs[i].Stage == trace.StageCapture {
			found = &recs[i]
		}
	}
	if found == nil {
		t.Fatalf("no capture.outcome record found: %+v", recs)
	}
	if found.Outcome != "recorded" {
		t.Errorf("Outcome = %q, want %q", found.Outcome, "recorded")
	}
}
