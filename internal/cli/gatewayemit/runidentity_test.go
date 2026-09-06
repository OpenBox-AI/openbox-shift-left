package gatewayemit

import (
	"context"
	"testing"
	"time"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
)

// TestEmitStampsRunIdentityFromTheSharedRecord is T4: a lane event's
// run_id/run_generation must be read from the SAME record a hook event would
// read (R7) -- and a pair whose observed start precedes the record's own
// bump timestamp must carry the PREVIOUS run id on BOTH halves (R12), never
// split across them.
func TestEmitStampsRunIdentityFromTheSharedRecord(t *testing.T) {
	em, spool, _ := newTestEmitter(t)
	runDir := t.TempDir()
	bumpedAt := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	store := obgit.RunStore{Dir: runDir, Now: func() time.Time { return bumpedAt }}
	rec, err := store.Bump("sess-1")
	if err != nil {
		t.Fatalf("seeding the run record: %v", err)
	}
	if rec.Generation != 1 {
		t.Fatalf("setup: generation = %d, want 1", rec.Generation)
	}
	em.RunStore = obgit.RunStore{Dir: runDir}

	t.Run("a call entirely after the bump carries the current run", func(t *testing.T) {
		c := capturedWithSession("sess-1")
		c.StartedAt = bumpedAt.Add(time.Second)
		c.EndedAt = bumpedAt.Add(2 * time.Second)
		em.Emit(context.Background(), c)

		events := spooledEvents(t, spool, "sess-1")
		if len(events) != 2 {
			t.Fatalf("spooled %d events, want 2", len(events))
		}
		for _, ev := range events {
			if ev.RunID != rec.RunID || ev.RunGeneration != rec.Generation {
				t.Errorf("event %s: run_id=%q run_generation=%d, want %q/%d (the hook-side record)",
					ev.EventType, ev.RunID, ev.RunGeneration, rec.RunID, rec.Generation)
			}
		}
	})

	t.Run("a call straddling the bump (started before, ended after) is NOT split -- both halves carry the PREVIOUS run", func(t *testing.T) {
		c := capturedWithSession("sess-1")
		c.StartedAt = bumpedAt.Add(-time.Second)
		c.EndedAt = bumpedAt.Add(time.Second)
		em.Emit(context.Background(), c)

		// This session's spool now holds both sub-tests' events; take the tail.
		events := spooledEvents(t, spool, "sess-1")
		if len(events) < 2 {
			t.Fatalf("spooled %d events, want at least 2", len(events))
		}
		tail := events[len(events)-2:]
		for _, ev := range tail {
			if ev.RunID != rec.PreviousRunID || ev.RunGeneration != rec.Generation-1 {
				t.Errorf("event %s: run_id=%q run_generation=%d, want the PREVIOUS run %q/%d",
					ev.EventType, ev.RunID, ev.RunGeneration, rec.PreviousRunID, rec.Generation-1)
			}
		}
		if tail[0].RunID != tail[1].RunID {
			t.Errorf("a straddling pair split across two run ids: %q vs %q", tail[0].RunID, tail[1].RunID)
		}
	})
}
