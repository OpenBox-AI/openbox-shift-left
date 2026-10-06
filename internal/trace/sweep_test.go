package trace

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func dayAt(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 15, 0, 0, 0, time.UTC)
}

func TestSweepMidnightCrossingGzipsYesterdayKeepsToday(t *testing.T) {
	dir := t.TempDir()
	day1 := dayAt(2026, 9, 26)
	w := &Writer{Dir: dir, Now: func() time.Time { return day1 }}

	w.Emit(Record{Stage: StageLog, Detail: map[string]any{"n": 1}})

	yesterdayFile := filepath.Join(dir, "trace-2026-09-26.jsonl")
	if _, err := os.Stat(yesterdayFile); err != nil {
		t.Fatalf("expected today's (day1) file to exist: %v", err)
	}

	// Cross midnight: same Writer, clock now reports the next day.
	day2 := dayAt(2026, 9, 27)
	w.Now = func() time.Time { return day2 }
	w.Emit(Record{Stage: StageLog, Detail: map[string]any{"n": 2}})

	todayFile := filepath.Join(dir, "trace-2026-09-27.jsonl")
	if _, err := os.Stat(todayFile); err != nil {
		t.Fatalf("expected new day's file to exist: %v", err)
	}
	if _, err := os.Stat(yesterdayFile); err != nil {
		t.Fatalf("yesterday's plain file should still exist before Sweep: %v", err)
	}

	if err := w.Sweep(); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if _, err := os.Stat(yesterdayFile); !os.IsNotExist(err) {
		t.Fatalf("yesterday's plain file should be gone after gzip, err=%v", err)
	}
	gz := yesterdayFile + ".gz"
	if _, err := os.Stat(gz); err != nil {
		t.Fatalf("expected gzip of yesterday, stat err: %v", err)
	}
	if _, err := os.Stat(todayFile); err != nil {
		t.Fatalf("today's plain file must survive Sweep: %v", err)
	}

	recs, skipped, err := Read(dir, nil)
	if err != nil {
		t.Fatalf("Read after sweep: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	if len(recs) != 2 {
		t.Fatalf("recs = %d, want 2", len(recs))
	}
}

func TestSweepSecondCallIsNoOp(t *testing.T) {
	dir := t.TempDir()
	day1 := dayAt(2026, 9, 26)
	w := &Writer{Dir: dir, Now: func() time.Time { return day1 }}
	w.Emit(Record{Stage: StageLog})

	day2 := dayAt(2026, 9, 27)
	w.Now = func() time.Time { return day2 }

	if err := w.Sweep(); err != nil {
		t.Fatalf("first Sweep: %v", err)
	}
	entriesAfterFirst, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	before := map[string]int64{}
	for _, e := range entriesAfterFirst {
		info, _ := e.Info()
		before[e.Name()] = info.ModTime().UnixNano()
	}

	if err := w.Sweep(); err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	entriesAfterSecond, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entriesAfterSecond) != len(entriesAfterFirst) {
		t.Fatalf("second sweep changed file count: before=%d after=%d", len(entriesAfterFirst), len(entriesAfterSecond))
	}
	for _, e := range entriesAfterSecond {
		info, _ := e.Info()
		if before[e.Name()] != info.ModTime().UnixNano() {
			t.Fatalf("second sweep re-touched %s (no-op expected)", e.Name())
		}
	}
}

func TestSweepPruneRetentionBoundary(t *testing.T) {
	dir := t.TempDir()
	today := dayAt(2026, 9, 27)

	// today-6 kept, today-7 deleted (RetainDays=7: today + 6 prior days).
	kept := today.AddDate(0, 0, -6)
	deleted := today.AddDate(0, 0, -7)

	for _, d := range []time.Time{kept, deleted} {
		name := "trace-" + d.Format(dateLayout) + ".jsonl"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"stage":"log"}`+"\n"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	w := &Writer{Dir: dir, Now: func() time.Time { return today }}
	if err := w.Sweep(); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	keptGz := filepath.Join(dir, "trace-"+kept.Format(dateLayout)+".jsonl.gz")
	if _, err := os.Stat(keptGz); err != nil {
		t.Fatalf("today-6 file should survive (gzipped): %v", err)
	}
	deletedPlain := filepath.Join(dir, "trace-"+deleted.Format(dateLayout)+".jsonl")
	deletedGz := deletedPlain + ".gz"
	if _, err := os.Stat(deletedPlain); !os.IsNotExist(err) {
		t.Fatalf("today-7 plain file should be deleted, err=%v", err)
	}
	if _, err := os.Stat(deletedGz); !os.IsNotExist(err) {
		t.Fatalf("today-7 gz file should be deleted, err=%v", err)
	}
}

func TestSweepSkipsWhenLockHeldByAnother(t *testing.T) {
	dir := t.TempDir()
	today := dayAt(2026, 9, 27)
	yesterday := today.AddDate(0, 0, -1)

	name := filepath.Join(dir, "trace-"+yesterday.Format(dateLayout)+".jsonl")
	if err := os.WriteFile(name, []byte(`{"stage":"log"}`+"\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	other := flock.New(filepath.Join(dir, ".rotate.lock"))
	locked, err := other.TryLock()
	if err != nil || !locked {
		t.Fatalf("failed to take competing lock: locked=%v err=%v", locked, err)
	}
	defer other.Unlock()

	w := &Writer{Dir: dir, Now: func() time.Time { return today }}
	if err := w.Sweep(); err != nil {
		t.Fatalf("Sweep should skip without error when lock is held, got: %v", err)
	}

	// Nothing should have been gzipped: the plain file is untouched.
	if _, err := os.Stat(name); err != nil {
		t.Fatalf("plain file should still exist untouched: %v", err)
	}
	if _, err := os.Stat(name + ".gz"); !os.IsNotExist(err) {
		t.Fatalf("no gz should have been produced while lock was held")
	}
}

func TestSweepPrunesOversizedDirectoryOldestFirstNeverToday(t *testing.T) {
	dir := t.TempDir()
	today := dayAt(2026, 9, 27)

	// Three past days already gzipped, each ~200MiB, plus today's plain file.
	// Total (~600MiB) exceeds the 512MiB budget, so the oldest past day must
	// be pruned first while today's file is never touched.
	chunk := make([]byte, 200<<20)
	days := []time.Time{
		today.AddDate(0, 0, -3),
		today.AddDate(0, 0, -2),
		today.AddDate(0, 0, -1),
	}
	for _, d := range days {
		name := filepath.Join(dir, "trace-"+d.Format(dateLayout)+".jsonl.gz")
		if err := os.WriteFile(name, chunk, 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	todayFile := filepath.Join(dir, "trace-"+today.Format(dateLayout)+".jsonl")
	if err := os.WriteFile(todayFile, []byte(`{"stage":"log"}`+"\n"), 0o600); err != nil {
		t.Fatalf("seed today file: %v", err)
	}

	w := &Writer{Dir: dir, Now: func() time.Time { return today }}
	if err := w.Sweep(); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	oldest := filepath.Join(dir, "trace-"+days[0].Format(dateLayout)+".jsonl.gz")
	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Fatalf("oldest past day should have been pruned for size, err=%v", err)
	}
	if _, err := os.Stat(todayFile); err != nil {
		t.Fatalf("today's file must never be pruned: %v", err)
	}
}
