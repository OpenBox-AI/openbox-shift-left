package hookflow

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// Emitter is the transport events are delivered through; satisfied by
// *client.Client.
type Emitter interface {
	Emit(ctx context.Context, ev client.DevEvent) (client.Evaluation, error)
}

// Engine owns everything a hook run does with an event once it has been
// mapped: spool it, bridge its start time to the paired completion, deliver
// it, and record the advisory verdict.
type Engine struct {
	Spool Spool
	// Advisory records the Advisory-tier verdict/guardrail signals on flush.
	// Record-only, off the hot path, never blocks (INV-3). Nil ⇒ no advisory
	// recording (still delivers, still never blocks).
	Advisory *Advisory
	// Durations bridges the PreToolUse start time to the paired PostToolUse so a
	// completed span computes a real cross-process duration.
	Durations DurationStash
	// Turns records how far each (session, agent) transcript window has been
	// consumed for per-turn usage extraction, so repeated turn-boundary hook
	// firings never re-report the same turn's tokens.
	Turns TurnCursor
	// Log reports what delivery could not do. Nil ⇒ silent, never in production.
	Log func(format string, args ...any)
}

func (e *Engine) logf(format string, args ...any) {
	if e.Log != nil {
		e.Log(format, args...)
	}
}

// NewEngine builds an Engine spooling under dir and writing Advisory records
// to the default developer-scoped sink.
func NewEngine(spoolDir string) *Engine {
	return &Engine{
		Spool:     Spool{Dir: spoolDir},
		Advisory:  &Advisory{Path: DefaultAdvisoryPath()},
		Durations: DurationStash{Dir: filepath.Join(spoolDir, "durations")},
		Turns:     TurnCursor{Dir: filepath.Join(spoolDir, "turns")},
	}
}

// Record is the hot path: thread the tool call's duration and append the event
// to the local spool. The returned error is a local spool-write failure the
// caller logs fail-open; it is never surfaced to the developer tool.
func (e *Engine) Record(ev client.DevEvent) error {
	e.ThreadDuration(&ev)
	return e.Spool.Append(ev)
}

// RecordDeferred is Record split at the point where delivery matters: it
// threads the duration NOW and returns the spool write as a closure, for a
// caller that does not yet know whether this same event is about to be
// delivered synchronously.
func (e *Engine) RecordDeferred(ev client.DevEvent) func() error {
	e.ThreadDuration(&ev)
	return func() error { return e.Spool.Append(ev) }
}

// ThreadDuration records/recovers a tool call's start time across the separate
// Pre/PostToolUse hook processes (DurationStash), mutating only ev.StartedAt
// on the completed (ToolResult) event; which the client turns into the
// completed span's start_time, and thus a non-zero duration_ns.
func (e *Engine) ThreadDuration(ev *client.DevEvent) {
	switch ev.EventType {
	case client.EventToolCall:
		_ = e.Durations.PutStart(ev.SessionID, ToolCallStartKey(*ev), ev.StartedAt)
	case client.EventToolResult:
		if start := e.Durations.TakeStart(ev.SessionID, ToolCallStartKey(*ev)); start != "" {
			ev.StartedAt = start
		}
	case client.EventSessionEnded:
		e.Durations.ClearSession(ev.SessionID)
		e.Turns.ClearSession(ev.SessionID)
	}
}

// Flush drains the given session's spooled events through the Emitter, then
// sweeps carry-over (recovery) files left undelivered by earlier flushes. It
// is bounded by ctx (the caller caps session-end flush so teardown is never
// delayed unduly).
//
// It owns the session's debounce lock, moved here from the two adapter callers
// because nothing could hold the choreography while it was duplicated: drainFile
// renames the file aside BEFORE delivering and Maybe returns silently on a fresh
// lock, so an append during a drain is invisible to both. 81 events sat that way.
func (e *Engine) Flush(ctx context.Context, sessionID string, em Emitter) (int, error) {
	fn := e.emitFunc(em)
	carried := e.Spool.recoveryFiles()

	total, err := e.drainUntilEmpty(ctx, sessionID, fn)

	swept, sweepErr := e.speaking().sweepRecovery(ctx, carried, sessionID, fn)
	if err == nil {
		err = sweepErr
	}
	return total + swept, err
}

func (e *Engine) drainUntilEmpty(ctx context.Context, sessionID string, fn FlushFunc) (int, error) {
	spool := e.speaking()
	total := 0
	var err error
	// Its own scope so the release is DEFERRED: a panic would strand the lock.
	func() {
		defer e.Spool.ReleaseFlushLock(sessionID)
		for pass := 1; ; pass++ {
			// Per pass, not once: a drain outliving the debounce window would otherwise
			// let Maybe's stale-lock takeover spawn a competing flusher mid-drain.
			e.Spool.TouchFlushLock(sessionID)

			var n int
			n, err = spool.FlushSession(ctx, sessionID, fn)
			total += n
			if err != nil {
				return
			}
			remaining := e.Spool.PendingCount(sessionID)
			if remaining == 0 {
				return
			}
			if pass >= MaxDrainPasses {
				e.logf("spool: %s: %d event(s) remain after %d drain passes; they stay spooled for the sweep",
					sessionID, remaining, MaxDrainPasses)
				return
			}
		}
	}()

	// One pass AFTER the release closes the sliver; a double drain is harmless.
	if err == nil && e.Spool.PendingCount(sessionID) > 0 {
		n, lateErr := spool.FlushSession(ctx, sessionID, fn)
		total += n
		err = lateErr
	}
	return total, err
}

// FlushAll drains every spooled session, each until empty as Flush does.
func (e *Engine) FlushAll(ctx context.Context, em Emitter) (int, error) {
	return e.speaking().FlushAll(ctx, e.emitFunc(em))
}

// speaking attaches the engine's voice to a COPY of its spool: two flushes can
// run concurrently, so assigning to e.Spool would be a data race.
func (e *Engine) speaking() Spool {
	s := e.Spool
	s.Log = e.Log
	return s
}

// FlushOrSweep is the whole of what a `flush` invocation does, so an adapter does
// not re-derive it. An empty sessionID sweeps every session, THEN retires: a
// deliverable file must be delivered rather than deleted for being old.
func (e *Engine) FlushOrSweep(ctx context.Context, sessionID string, em Emitter) (int, error) {
	if sessionID != "" {
		return e.Flush(ctx, sessionID, em)
	}
	n, err := e.FlushAll(ctx, em)
	if ctx.Err() != nil {
		// The walk stopped partway, so files behind it were never opened -- and a
		// stale mtime on one of those means nobody TRIED for the retention age, not
		// that the age was spent failing to deliver. A flusher that was simply down
		// for a month leaves exactly that, and its first pass back would delete
		// still-deliverable evidence without ever attempting it.
		e.logf("spool: the sweep ended early (%v); skipping retirement, because a file "+
			"this pass never reached must not be deleted for being old", err)
		return n, err
	}
	if err != nil {
		// A completed walk opened every file, and a refusal never arrives here at all
		// -- drainRotated re-spools and reports only a cut pass -- so this is a
		// rotate or read fault on one file. It is no reason to skip the others:
		// gating on it was the half of the old rule that protected nothing.
		e.logf("spool: the sweep reported %v; retirement still runs, because the walk "+
			"completed and every file got its attempt", err)
	}

	// Retirement gets its OWN budget: sharing one deadline meant a heavy backlog
	// spent it all, so the machines that most needed retirement never got it.
	retireCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), retireBudget)
	defer cancel()
	// Joined, not assigned: this line is now reachable with err already non-nil,
	// and the flush failure is the one that explains the partial pass.
	if _, retireErr := e.Retire(retireCtx); retireErr != nil {
		err = errors.Join(err, retireErr)
	}
	return n, err
}

// retireBudget bounds the retirement pass. Generous for what it does -- a ReadDir,
// a line count and an unlink per stale file -- and unrelated to the flush budget.
const retireBudget = 5 * time.Second

// Retire deletes spool files too old to deliver, loudly. ctx bounds it per file
// and not merely on entry, which is what makes retireBudget an actual bound.
func (e *Engine) Retire(ctx context.Context) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	retired, err := e.speaking().RetireStale(ctx, time.Now(), RetireSpoolAfter)
	for _, r := range retired {
		e.logf("spool: RETIRED %s: past the %d-day retention age, the events in it were never delivered "+
			"and are now gone", r, int(RetireSpoolAfter.Hours()/24))
	}
	if err != nil {
		e.logf("spool: retirement incomplete: %v", err)
	}
	return len(retired), err
}

// emitFunc adapts an Emitter to a FlushFunc. It emits the event and then
// records the returned Evaluation to the Advisory sink; record-only, never
// blocking.
func (e *Engine) emitFunc(em Emitter) FlushFunc {
	return func(ctx context.Context, ev client.DevEvent) error {
		eval, err := em.Emit(ctx, ev)
		// On success, a real verdict is recorded. Either way this cannot block the
		// tool call.
		e.Advisory.Record(ev, eval)
		return err
	}
}
