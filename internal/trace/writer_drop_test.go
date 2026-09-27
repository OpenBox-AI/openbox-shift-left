package trace

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Every Emit failure counts exactly one drop and never escapes; the empty-dir
// and mkdir cases live in writer_test.go.
func TestEmitCountsOneDropPerFailure(t *testing.T) {
	fixed := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return fixed }

	cases := map[string]func(t *testing.T) (*Writer, Record){
		"marshal": func(t *testing.T) (*Writer, Record) {
			return &Writer{Dir: t.TempDir(), Now: clock}, Record{Stage: StageLog, Detail: map[string]any{"c": make(chan int)}}
		},
		"open": func(t *testing.T) (*Writer, Record) {
			dir := t.TempDir()
			// A directory where today's trace file should be fails the open.
			if err := os.Mkdir(filepath.Join(dir, "trace-2026-09-27.jsonl"), 0o700); err != nil {
				t.Fatal(err)
			}
			return &Writer{Dir: dir, Now: clock}, Record{Stage: StageLog}
		},
		"panic": func(t *testing.T) (*Writer, Record) {
			return &Writer{Dir: t.TempDir(), Now: func() time.Time { panic("clock") }}, Record{Stage: StageLog}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			w, r := setup(t)
			w.Emit(r)
			if w.dropped != 1 {
				t.Fatalf("dropped = %d, want 1", w.dropped)
			}
			w.Emit(r)
			if w.dropped != 2 {
				t.Fatalf("dropped after a second failure = %d, want 2", w.dropped)
			}
		})
	}
}

func TestEmitSuccessCountsNoDrop(t *testing.T) {
	w := &Writer{Dir: t.TempDir()}
	w.Emit(Record{Stage: StageLog})
	if w.dropped != 0 {
		t.Fatalf("dropped = %d, want 0", w.dropped)
	}
}
