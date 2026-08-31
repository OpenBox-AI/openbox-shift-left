package hookflow

import (
	"os"
	"testing"
	"testing/synctest"
	"time"
)

// bubbleStampLock puts the lock's modification time onto the bubble's clock.
// The debounce reads time.Since(mtime), and inside a bubble time.Now() is the
// fake clock while a freshly created file carries a real wall-clock mtime --
// two clocks decades apart, which makes every comparison meaningless. Only the
// O_EXCL create path needs this; the takeover path stamps the lock with
// time.Now() itself, so it is already on the bubble's clock.
func bubbleStampLock(t *testing.T, path string) {
	t.Helper()
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatalf("stamp lock: %v", err)
	}
}

// TestRealtimeDebounceHoldsTheDefaultWindowAtBothEdges. The existing debounce
// tests either burst with no elapsed time or shorten Window to 50ms, so the
// shipped 2s window has never been exercised: waiting it out for real costs
// two seconds of suite time per assertion, which is exactly why nobody did.
// Fake time makes both edges free.
//
// The probes sit well clear of the edge because a filesystem may truncate
// mtime to whole seconds, which can only make time.Since read *larger*: at
// 500ms elapsed the worst case is 1.5s, still inside a 2s window, and at 4s
// the worst case is 4s, still outside it.
func TestRealtimeDebounceHoldsTheDefaultWindowAtBothEdges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr, spawned := testTrigger(t, true)
		if tr.Window != 0 {
			t.Fatalf("Window = %v, want the zero value so DefaultRealtimeWindow applies", tr.Window)
		}

		tr.Maybe(discard(), "sess-1")
		if len(*spawned) != 1 {
			t.Fatalf("first Maybe spawned %d flushers, want 1", len(*spawned))
		}
		bubbleStampLock(t, tr.Spool.FlushLockPath("sess-1"))

		time.Sleep(DefaultRealtimeWindow / 4)
		tr.Maybe(discard(), "sess-1")
		if len(*spawned) != 1 {
			t.Errorf("a second Maybe %v into a %v window spawned again (%d total); the window exists so a "+
				"burst of tool calls is delivered by one drain, not one process per hook",
				DefaultRealtimeWindow/4, DefaultRealtimeWindow, len(*spawned))
		}

		time.Sleep(2 * DefaultRealtimeWindow)
		tr.Maybe(discard(), "sess-1")
		if len(*spawned) != 2 {
			t.Errorf("Maybe %v past the lock spawned %d flushers in total, want 2: past the window the "+
				"lock is stale and a session whose flusher died would never be drained again",
				DefaultRealtimeWindow/4+2*DefaultRealtimeWindow, len(*spawned))
		}

		// The takeover restamped the lock, so the new window runs from there.
		time.Sleep(DefaultRealtimeWindow / 4)
		tr.Maybe(discard(), "sess-1")
		if len(*spawned) != 2 {
			t.Errorf("the window did not restart after a takeover: %d spawns, want 2", len(*spawned))
		}
	})
}
