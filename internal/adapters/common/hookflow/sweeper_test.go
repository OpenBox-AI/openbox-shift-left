package hookflow

import (
	"bytes"
	"context"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// spawnRecorder captures what the sweeper would have launched.
type spawnRecorder struct {
	mu   sync.Mutex
	argv [][]string
}

func (r *spawnRecorder) start(cmd *exec.Cmd) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.argv = append(r.argv, append([]string(nil), cmd.Args...))
	return nil
}

func (r *spawnRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.argv)
}

func newSweeper(t *testing.T, spool Spool, rec *spawnRecorder) Sweeper {
	t.Helper()
	return Sweeper{
		Spool:    spool,
		Provider: "claude-code",
		Self:     filepath.Join(t.TempDir(), "openbox"),
		Interval: time.Hour, // one sweep per Run in these tests
		Delay:    -1,        // sweep immediately
		Start:    rec.start,
	}
}

// TestTheSweepSpawnsACatchUpFlushForABacklog is the completeness net: nothing
// periodic or at-startup ever drained an abandoned session file, so one
// accumulated 1,048 events and sat untouched for eighteen days.
func TestTheSweepSpawnsACatchUpFlushForABacklog(t *testing.T) {
	e := testEngine(t)
	spoolEvent(t, e, "abandoned", "a")

	rec := &spawnRecorder{}
	var logged bytes.Buffer
	logger := log.New(&logged, "", 0)

	ctx, cancel := context.WithCancel(context.Background())
	sw := newSweeper(t, e.Spool, rec)
	go func() { sw.Run(ctx, logger); cancel() }()
	waitFor(t, func() bool { return rec.count() > 0 })
	cancel()

	argv := rec.argv[0]
	if got := strings.Join(argv[1:], " "); got != "hook claude-code flush" {
		t.Errorf("sweep spawned %q, want the no-session flush (which is the FlushAll path)", got)
	}
	if !strings.Contains(logged.String(), "1 spooled event") {
		t.Errorf("the sweep did not report what it found: %q", logged.String())
	}
}

// TestTheSweepIsSilentOnAnEmptySpool an empty queue is the healthy steady state,
// and a net that spawns a process every interval regardless would be a cost with
// no purpose and a log nobody could read.
func TestTheSweepIsSilentOnAnEmptySpool(t *testing.T) {
	e := testEngine(t)

	rec := &spawnRecorder{}
	var logged bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	sw := newSweeper(t, e.Spool, rec)
	go sw.Run(ctx, log.New(&logged, "", 0))
	time.Sleep(200 * time.Millisecond)
	cancel()

	if rec.count() != 0 {
		t.Errorf("the sweep spawned %d flusher(s) for an empty spool", rec.count())
	}
	if logged.String() != "" {
		t.Errorf("the sweep logged on an empty spool: %q", logged.String())
	}
}

// TestTheSweepStopsWithItsContext a daemon's shutdown must not leave a ticker
// spawning flushers.
func TestTheSweepStopsWithItsContext(t *testing.T) {
	e := testEngine(t)
	spoolEvent(t, e, "s", "a")

	rec := &spawnRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	sw := newSweeper(t, e.Spool, rec)
	go func() { sw.Run(ctx, log.New(&bytes.Buffer{}, "", 0)); close(done) }()

	waitFor(t, func() bool { return rec.count() > 0 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// TestBacklogCountSeesPlainSessionFiles is the distinction that made a field
// designed to report undelivered evidence read 71 while 118 events sat in plain
// session files: UndeliveredCount counts carry-over files ONLY.
func TestBacklogCountSeesPlainSessionFiles(t *testing.T) {
	e := testEngine(t)
	for i := range 5 {
		spoolEvent(t, e, "plain", string(rune('a'+i)))
	}

	if got := e.Spool.UndeliveredCount(); got != 0 {
		t.Errorf("UndeliveredCount = %d; it counts carry-over files only, by design", got)
	}
	if got := e.Spool.BacklogCount(); got != 5 {
		t.Errorf("BacklogCount = %d, want 5; a plain session file is still undelivered evidence", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within the deadline")
}
