package hookflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// TestARotatedFileIsStampedBeforeTheLockDrops: the double-delivery race this
// guarded against used to be closed by a claim-then-stamp rename on a stray
// `.flushing.` file (see git history); DrainSession's own per-session stripe
// lock now makes a concurrent reclaim of the SAME stem structurally
// impossible instead (only one drainer can be inside reclaimStemOrphans for a
// given stem at a time), so that race no longer needs its own regression test
// here -- sessionorder_test.go's -race, 200-iteration drain covers the general
// property. What still needs pinning one layer up: collectSession stamps a
// live drain's OWN rotated file under the spool lock, so a concurrent FlushAll
// walk can never read it as old enough to be an orphan while delivery is still
// in flight.

func TestARotatedFileIsStampedBeforeTheLockDrops(t *testing.T) {
	e := testEngine(t)
	spoolEvent(t, e, "aged", "aged-1")

	path := e.Spool.SessionPath("aged")
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	// Observe the rotated file from inside the drain, which is exactly where a
	// concurrent walk would see it.
	var sawAge time.Duration
	seen := false
	fn := func(_ context.Context, _ client.DevEvent) error {
		entries, err := os.ReadDir(e.Spool.Dir)
		if err != nil {
			return nil
		}
		for _, en := range entries {
			if !strings.Contains(en.Name(), ".flushing.") {
				continue
			}
			if info, err := en.Info(); err == nil {
				sawAge, seen = time.Since(info.ModTime()), true
			}
		}
		return nil
	}
	if _, err := e.Spool.DrainSession(context.Background(), "aged", fn, DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout}); err != nil {
		t.Fatalf("DrainSession: %v", err)
	}
	if !seen {
		t.Skip("the rotated file was not observable from the delivery callback")
	}
	if sawAge >= ReclaimOrphanAfter {
		t.Errorf("a live drain's rotated file reads as %v old, past the %v threshold; a "+
			"concurrent sweep would reclaim it", sawAge.Round(time.Second), ReclaimOrphanAfter)
	}
}

// TestACorruptSpooledLineIsRecordedAsLost was the one loss in the drain loop that
// left no trace: not delivered, not carried over, not counted, and the source
// unlinked -- so every counter read zero and the session still reported
// evidence_state "complete". A torn tail from a crash mid-Append is that shape.
func TestACorruptSpooledLineIsRecordedAsLost(t *testing.T) {
	e := testEngine(t)
	path := e.Spool.SessionPath("torn")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schema_version":"1.7","event_ty`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	delivered := 0
	fn := func(_ context.Context, _ client.DevEvent) error { delivered++; return nil }
	if _, err := e.Spool.DrainSession(context.Background(), "torn", fn, DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout}); err != nil {
		t.Fatalf("DrainSession: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("precondition: a torn line was delivered %d time(s)", delivered)
	}
	if got := e.Spool.DiscardedCount(); got != 1 {
		t.Errorf("DiscardedCount = %d, want 1: a line that will not parse is still a loss", got)
	}
}
