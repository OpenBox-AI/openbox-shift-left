package trace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// Writer is one trace sink: a directory, a clock (overridable so rotation
// tests can cross midnight without sleeping), and the process name stamped
// on every record it emits. The package-level Emit/SetDir/SetProcess/Dropped
// functions operate on a default *Writer; SetDefault swaps it out entirely,
// which is the seam tests use to point at a scratch directory and a fake
// clock without touching global mutable state directly.
type Writer struct {
	Dir  string
	Now  func() time.Time
	Proc string

	// dropped counts write failures on this Writer. Unexported: the exported
	// surface is the package-level Dropped() reading the current default.
	dropped uint64
}

// defaultW is the process-wide default Writer, swappable via SetDefault.
var defaultW atomic.Pointer[Writer]

func init() {
	defaultW.Store(&Writer{})
}

// SetProcess stamps Proc on every record the default writer emits from here
// on. It replaces the default writer with a copy carrying the new name
// rather than mutating fields in place, so a concurrent Emit reading the
// previous instance never observes a half-updated Writer.
func SetProcess(name string) {
	swapDefault(func(w Writer) Writer {
		w.Proc = name
		return w
	})
}

// SetDir sets the default writer's directory. Empty means every Emit is a
// silent, uncounted discard -- the safety net that keeps a test or a tool
// that never called SetDir from writing anywhere.
func SetDir(dir string) {
	swapDefault(func(w Writer) Writer {
		w.Dir = dir
		return w
	})
}

// Dir returns the default writer's current directory ("" until SetDir).
func Dir() string {
	return defaultW.Load().Dir
}

// Dropped returns the number of Emit failures on the current default writer
// in this process. It reflects whichever Writer is live right now: a
// SetDefault swap starts a fresh count on the new instance.
func Dropped() uint64 {
	return atomic.LoadUint64(&defaultW.Load().dropped)
}

// swapDefault installs a new default Writer built by mutating a snapshot of
// the current one, so SetDir/SetProcess never race a concurrent Emit's field
// reads on the live instance.
func swapDefault(mutate func(Writer) Writer) {
	old := defaultW.Load()
	next := mutate(Writer{
		Dir:     old.Dir,
		Now:     old.Now,
		Proc:    old.Proc,
		dropped: atomic.LoadUint64(&old.dropped),
	})
	defaultW.Store(&next)
}

// SetDefault replaces the default writer wholesale -- the test seam: point
// Emit/Dropped/Dir at a throwaway Writer (scratch dir, fake clock) without
// disturbing whatever the real process configured, then restore it.
func SetDefault(w *Writer) (restore func()) {
	old := defaultW.Load()
	if w == nil {
		w = &Writer{}
	}
	defaultW.Store(w)
	return func() { defaultW.Store(old) }
}

// Emit writes r through the default writer. See (*Writer).Emit for the
// guarantees (never blocks long, never panics, swallows errors).
func Emit(r Record) {
	defaultW.Load().Emit(r)
}

// Emit appends one JSONL line to today's trace file: OpenFile(O_CREATE|
// O_WRONLY|O_APPEND, 0600) + a single Write of the full line + Close, so
// concurrent writers across processes never interleave partial lines without
// needing a per-write lock. TS/PID/Proc are always overwritten from the
// writer's own clock/pid/name -- a caller cannot spoof provenance.
//
// This must never cost a hook process its 30s budget or crash it, so every
// failure (marshal, mkdir, open, write) is caught, counted in dropped, and
// swallowed; a bare Emit dir=="" is an even cheaper no-op that isn't counted
// at all, since that's the "nobody called SetDir yet" case rather than a
// failure.
func (w *Writer) Emit(r Record) {
	if w == nil || w.Dir == "" {
		return
	}
	if w.write(r) != nil {
		atomic.AddUint64(&w.dropped, 1)
	}
}

// write stamps r and appends it as one line; a panic comes back as an error
// so Emit counts it like any other failure.
func (w *Writer) write(r Record) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("trace: emit panicked: %v", p)
		}
	}()

	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	ts := now().UTC()

	r.TS = ts
	r.PID = os.Getpid()
	r.Proc = w.Proc

	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	if err := os.MkdirAll(w.Dir, 0o700); err != nil {
		return err
	}

	name := "trace-" + ts.Format("2006-01-02") + ".jsonl"
	f, err := os.OpenFile(filepath.Join(w.Dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.Write(line)
	return err
}
