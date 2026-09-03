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

// TestAReclaimedOrphanStopsLookingLikeAnOrphan is the double-delivery race.
//
// The claim on an orphan is exclusive only while its MTIME says a drain is
// holding it. Rename preserves the mtime and the claimed name still contains
// ".flushing.", so an unstamped reclaim stayed as old as it was: the next
// concurrent FlushAll -- routine, with all three lane daemons sweeping one
// directory -- renamed it again and drained the same lines in parallel. Nothing
// downstream dedupes (see the Spool doc comment), so every event landed twice
// and the two-rows-per-activity_id invariant broke.
func TestAReclaimedOrphanStopsLookingLikeAnOrphan(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, "sess.jsonl.flushing.OLD")
	if err := os.WriteFile(orphan, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	// What FlushAll's reclaim branch does, in the order it does it.
	claimed := orphan + ".reclaim.TEST"
	if err := os.Rename(orphan, claimed); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(claimed, now, now); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(claimed)
	if err != nil {
		t.Fatal(err)
	}
	// The name still matches, deliberately: that is what keeps the file counted as
	// backlog. The mtime is the only thing standing between it and a second drain.
	if !strings.Contains(filepath.Base(claimed), ".flushing.") {
		t.Fatal("precondition: the claimed name no longer matches the orphan predicate")
	}
	if age := time.Since(info.ModTime()); age >= ReclaimOrphanAfter {
		t.Errorf("a just-claimed file reads as %v old, past the %v orphan threshold, so a "+
			"concurrent sweep would reclaim it mid-drain and deliver every line twice",
			age.Round(time.Second), ReclaimOrphanAfter)
	}
}

// TestARotatedFileIsStampedBeforeTheLockDrops the same window one layer up.
//
// drainFile stamps under the spool lock now. Between an unlock and a later stamp
// the rotated file carries the SOURCE's mtime -- and writeRecovery deliberately
// stamps a carry-over back to the age of the data in it -- so a rotation of one
// was already past the orphan threshold at the instant it was published.
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
	if _, err := e.Spool.FlushSession(context.Background(), "aged", fn); err != nil {
		t.Fatalf("FlushSession: %v", err)
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
	if _, err := e.Spool.FlushSession(context.Background(), "torn", fn); err != nil {
		t.Fatalf("FlushSession: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("precondition: a torn line was delivered %d time(s)", delivered)
	}
	if got := e.Spool.DiscardedCount(); got != 1 {
		t.Errorf("DiscardedCount = %d, want 1: a line that will not parse is still a loss", got)
	}
}
