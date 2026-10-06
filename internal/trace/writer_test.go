package trace

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEmitDiscardsSilentlyWhenDirEmpty(t *testing.T) {
	restore := SetDefault(&Writer{}) // Dir left empty on purpose
	defer restore()

	Emit(Record{Stage: StageLog})
	if got := Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0 (empty dir is a silent discard, not a failure)", got)
	}
}

func TestEmitWritesLineWithMode0600(t *testing.T) {
	dir := t.TempDir()
	fixed := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	w := &Writer{Dir: dir, Now: func() time.Time { return fixed }, Proc: "openbox"}

	w.Emit(Record{Stage: StageProcStart, SessionID: "sess-1"})

	path := filepath.Join(dir, "trace-2026-09-27.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat trace file: %v", err)
	}
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("file mode = %v, want 0600", mode)
		}
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}
	var rec Record
	line := bytes.TrimSpace(b)
	if err := json.Unmarshal(line, &rec); err != nil {
		t.Fatalf("unmarshal record: %v", err)
	}
	if rec.Stage != StageProcStart || rec.SessionID != "sess-1" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if !rec.TS.Equal(fixed) {
		t.Fatalf("TS = %v, want %v", rec.TS, fixed)
	}
	if rec.PID != os.Getpid() {
		t.Fatalf("PID = %d, want %d", rec.PID, os.Getpid())
	}
	if rec.Proc != "openbox" {
		t.Fatalf("Proc = %q, want openbox", rec.Proc)
	}
	if !strings.HasSuffix(string(b), "\n") {
		t.Fatalf("line not newline-terminated")
	}
}

func TestEmitUnwritableDirCountsDroppedNoPanic(t *testing.T) {
	base := t.TempDir()
	// A regular file where a directory is expected: MkdirAll must fail on it.
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}
	dir := filepath.Join(blocker, "trace")

	restore := SetDefault(&Writer{Dir: dir})
	defer restore()

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Emit panicked: %v", r)
			}
		}()
		Emit(Record{Stage: StageLog})
	}()

	if got := Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1", got)
	}
}

func TestEmitConcurrentGoroutinesAllParseAndCountExact(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{Dir: dir, Now: time.Now, Proc: "openbox"}

	const goroutines = 32
	const perGoroutine = 25
	body := strings.Repeat("x", 300<<10) // ~300KiB, under the per-body cap

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				w.Emit(Record{
					Stage:     StageCapture,
					SessionID: "sess",
					Detail:    map[string]any{"body": Body(body), "g": g, "i": i},
				})
			}
		}(g)
	}
	wg.Wait()

	total := 0
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("open %s: %v", e.Name(), err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
		for sc.Scan() {
			line := sc.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var rec Record
			if err := json.Unmarshal(line, &rec); err != nil {
				t.Fatalf("line failed to parse: %v\nline=%s", err, line)
			}
			total++
		}
		if err := sc.Err(); err != nil {
			t.Fatalf("scan %s: %v", e.Name(), err)
		}
		f.Close()
	}

	want := goroutines * perGoroutine
	if total != want {
		t.Fatalf("parsed %d lines, want %d", total, want)
	}
	if d := w.dropped; d != 0 {
		t.Fatalf("dropped = %d, want 0", d)
	}
}

func TestSetProcessStampsProc(t *testing.T) {
	dir := t.TempDir()
	fixed := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	restore := SetDefault(&Writer{Dir: dir, Now: func() time.Time { return fixed }})
	defer restore()

	SetProcess("hook")
	Emit(Record{Stage: StageHookIn})

	recs, skipped, err := Read(dir, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	if len(recs) != 1 || recs[0].Proc != "hook" {
		t.Fatalf("recs = %+v, want one record with Proc=hook", recs)
	}
}

func TestDirReflectsSetDir(t *testing.T) {
	restore := SetDefault(&Writer{})
	defer restore()

	if got := Dir(); got != "" {
		t.Fatalf("Dir() = %q, want empty before SetDir", got)
	}
	SetDir("/some/dir")
	if got := Dir(); got != "/some/dir" {
		t.Fatalf("Dir() = %q, want /some/dir", got)
	}
}
