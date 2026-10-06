package hookflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RetireSpoolAfter is how long an undeliverable file is kept. It errs long:
// deleting evidence is irreversible.
const RetireSpoolAfter = 30 * 24 * time.Hour

const MaxDrainPasses = 8

// MaxRecoveryAttempts is retained only so doctor's existing wording still
// compiles and reads sensibly: this release has no carry-over/retry concept, so no code path here counts
// against it any more.
const MaxRecoveryAttempts = 5

// PendingCount reports how many events are currently queued for a session --
// its tail plus its head file combined -- the two places DrainSession still
// has left to deliver from. A single-attempt failure is not counted here: it
// was attempted and is gone (ledgered), not pending.
func (s Spool) PendingCount(sessionID string) int {
	return countLines(s.SessionPath(sessionID)) + countLines(s.headPath(sessionID))
}

func countLines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return len(NonEmptyLines(data))
}

// BacklogCount reports every waiting event; UndeliveredCount read 71 while 118
// sat invisible to it.
func (s Spool) BacklogCount() int {
	return s.sumLines(IsBacklogFile)
}

// IsBacklogFile reports whether a spool filename holds waiting events. The
// `.flushing.` case is not a detail: a flusher killed mid-drain leaves events in a
// file that does NOT end in `.jsonl`, which made the sweeper's gate read zero.
func IsBacklogFile(name string) bool {
	return strings.HasSuffix(name, ".jsonl") || strings.Contains(name, ".flushing.")
}

type Retired struct {
	Name   string
	Events int
	Age    time.Duration
}

func (r Retired) String() string {
	return fmt.Sprintf("%s (%d event(s), %d days old)", r.Name, r.Events, int(r.Age.Hours()/24))
}

// RetireStale deletes spool files past `age`.
//
// ctx is honoured per file, not merely on entry, or the budget its caller builds
// bounds nothing: a ReadDir plus a lock, a stat, a full-file line count and an
// unlink per stale file runs as long as the directory is deep, and retireOne's
// flock can stall behind a concurrent drain.
func (s Spool) RetireStale(ctx context.Context, now time.Time, age time.Duration) ([]Retired, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("spool readdir: %w", err)
	}
	var out []Retired
	var errs []error
	for _, e := range entries {
		if ctx.Err() != nil {
			errs = append(errs, fmt.Errorf("spool retire: %w", ctx.Err()))
			break
		}
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") || strings.Contains(name, ".flushing.") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < age {
			continue
		}
		path := filepath.Join(s.Dir, name)
		events, elapsed, removed, err := s.retireOne(path, now, age)
		if err != nil {
			errs = append(errs, fmt.Errorf("spool retire %s: %w", name, err))
			continue
		}
		if !removed {
			continue
		}
		s.recordDiscard(path, events,
			fmt.Sprintf("past the %d-day retention age, never delivered", int(age.Hours()/24)))
		traceSpoolRetire(strings.TrimSuffix(name, ".jsonl"), events, int(elapsed.Hours()/24))
		out = append(out, Retired{Name: name, Events: events, Age: elapsed})
	}
	s.retireStaleStartedMarkers(now, age)
	return out, errors.Join(errs...)
}

// retireStaleStartedMarkers sweeps the WorkflowStarted-accepted markers
// (spool.go's markWorkflowStarted/WorkflowStartedAccepted) past age: cheap
// (a ReadDir plus a Stat and an unlink per stale marker, no line-counting,
// no discard ledger -- a marker is not delivery evidence) and best-effort,
// piggybacked on the same retirement pass rather than a separate sweep. A
// marker this misses simply falls back to "unproven"
// (WorkflowStartedAccepted reports false), the same as if it had never been
// written.
func (s Spool) retireStaleStartedMarkers(now time.Time, age time.Duration) {
	entries, err := os.ReadDir(s.startedDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < age {
			continue
		}
		_ = os.Remove(filepath.Join(s.startedDir(), e.Name()))
	}
}

// retireOne re-checks the age and unlinks under the SPOOL LOCK every other
// mutator takes; without it an Append after the scan is deleted, uncounted.
func (s Spool) retireOne(path string, now time.Time, age time.Duration) (events int, elapsed time.Duration, removed bool, err error) {
	unlock := s.lockSpool()
	defer unlock()

	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, false, nil // delivered or retired by someone else: benign
	}
	elapsed = now.Sub(info.ModTime())
	if elapsed < age {
		return 0, 0, false, nil // appended to since the scan, so it is live again
	}
	events = countLines(path)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return 0, 0, false, nil
		}
		return 0, 0, false, err
	}
	return events, elapsed, true, nil
}
