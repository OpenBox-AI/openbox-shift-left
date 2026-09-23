package hookflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rawLine is a spool line without the trailing newline drainRotated's
// NonEmptyLines would strip, matching what writeRecovery expects.
func rawLine(t *testing.T, session, id string) []byte {
	t.Helper()
	line, err := jsonLine(ev(session, id))
	if err != nil {
		t.Fatal(err)
	}
	return line[:len(line)-1] // drop the trailing '\n' jsonLine appends
}

// TestSpoolDiscardAllRemovesEverySpooledFile guarantees a legacy
// identity's whole backlog -- an ordinary session file, a carried-over
// recovery file, and an in-flight rotated (".flushing.") file left by a
// killed drain -- is removed in one call, counted once, and recorded in the
// discard ledger under the given reason.
func TestSpoolDiscardAllRemovesEverySpooledFile(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}

	if err := sp.Append(ev("sessA", "a1")); err != nil {
		t.Fatalf("append a1: %v", err)
	}
	if err := sp.Append(ev("sessA", "a2")); err != nil {
		t.Fatalf("append a2: %v", err)
	}
	sp.writeRecovery(sp.SessionPath("sessB"), [][]byte{rawLine(t, "sessB", "b1")}, 1, time.Now())
	rotated := sp.SessionPath("sessC") + ".flushing.reclaim123"
	if err := os.WriteFile(rotated, rawLine(t, "sessC", "c1"), 0o600); err != nil {
		t.Fatalf("seed rotated file: %v", err)
	}

	if got := sp.BacklogCount(); got != 4 {
		t.Fatalf("backlog before discard = %d, want 4", got)
	}

	n, err := sp.DiscardAll("queued under the previous identity")
	if err != nil {
		t.Fatalf("DiscardAll: %v", err)
	}
	if n != 4 {
		t.Fatalf("discarded = %d, want 4", n)
	}
	if got := sp.BacklogCount(); got != 0 {
		t.Fatalf("backlog after discard = %d, want 0", got)
	}
	if got := sp.DiscardedCount(); got != 4 {
		t.Fatalf("discard ledger count = %d, want 4", got)
	}
	data, err := os.ReadFile(sp.DiscardPath())
	if err != nil {
		t.Fatalf("read discard ledger: %v", err)
	}
	if !strings.Contains(string(data), "queued under the previous identity") {
		t.Errorf("ledger does not carry the reason: %s", data)
	}
}

// TestSpoolDiscardAllOnMissingDirDiscardsNothing an uninitialized spool (no
// tool has ever appended to it) must not error: nothing was ever queued, so
// there is nothing to discard.
func TestSpoolDiscardAllOnMissingDirDiscardsNothing(t *testing.T) {
	sp := Spool{Dir: filepath.Join(t.TempDir(), "does-not-exist-yet")}
	n, err := sp.DiscardAll("reason")
	if err != nil || n != 0 {
		t.Fatalf("DiscardAll on a missing dir = (%d,%v), want (0,nil)", n, err)
	}
}

// TestSpoolDiscardAllOnEmptyDirRecordsNothing an empty spool discards zero
// events and must not write a ledger line for it (recordDiscard's own
// events<=0 guard, exercised here through the public entry point).
func TestSpoolDiscardAllOnEmptyDirRecordsNothing(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	n, err := sp.DiscardAll("reason")
	if err != nil || n != 0 {
		t.Fatalf("DiscardAll on an empty dir = (%d,%v), want (0,nil)", n, err)
	}
	if _, err := os.Stat(sp.DiscardPath()); !os.IsNotExist(err) {
		t.Errorf("an empty discard wrote a ledger file, stat err=%v", err)
	}
}
