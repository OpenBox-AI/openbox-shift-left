package hookflow

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"strings"
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
//
// Its Spool.OnFailure is wired here, once, to the delivery-failure finding:
// the ledger line every drainer already wrote (defaultOnFailure, unchanged),
// plus RecordDeliveryFailure's own log line and trace record -- never a run
// latch. Every drainer built on this Engine
// (the hook flusher, the periodic sweep, a lane daemon's own queue, a gate's
// own drain) inherits it from this one place; nothing downstream sets
// Spool.OnFailure again.
func NewEngine(spoolDir string) *Engine {
	e := &Engine{
		Spool:     Spool{Dir: spoolDir},
		Advisory:  &Advisory{Path: DefaultAdvisoryPath()},
		Durations: DurationStash{Dir: filepath.Join(spoolDir, "durations")},
		Turns:     TurnCursor{Dir: filepath.Join(spoolDir, "turns")},
	}
	e.Spool.OnFailure = func(ev client.DevEvent, err error) {
		// e.speaking(), not e.Spool directly: it is the copy whose Log is
		// wired from e.Log AT CALL TIME (e.Log may be assigned after
		// NewEngine returns), the same copy every drain path already logs
		// through.
		e.speaking().defaultOnFailure(ev, err)
		RecordDeliveryFailure(log.New(logfWriter{logf: e.logf}, "", 0), ev, err)
	}
	return e
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
// Pre/PostToolUse hook processes (DurationStash), mutating the completed
// (ToolResult) event's ev.StartedAt -- which the client turns into the
// completed span's start_time, and thus a non-zero duration_ns -- and, when
// the started half's operation id differs from the completed half's own
// mapped one, ev.Span.OperationID too, so the two halves still resolve to one
// activity_id (activityIDFor, client/payload.go) instead of tearing the call
// in two. An MCP call's Pre and Post hook processes can each map a different
// tool_input, which is exactly when this divergence happens; the started
// half's own operation id is never touched here or anywhere else in this
// function -- it is the approval key, derived from what THAT hook process
// actually saw, and must stay that way for a retry to consume an approval.
//
// When adoption changes the id, ev.Metadata["pair_recovered"] records that the
// two halves' mapped arguments actually disagreed (never `false`; simply
// absent otherwise), so how often that happens is measurable after ship.
func (e *Engine) ThreadDuration(ev *client.DevEvent) {
	switch ev.EventType {
	case client.EventToolCall:
		var opID string
		if ev.Span != nil {
			opID = ev.Span.OperationID
		}
		_ = e.Durations.putPair(ev.SessionID, pairKey(*ev), pairRecord{StartedAt: ev.StartedAt, OperationID: opID})
	case client.EventToolResult:
		rec := e.Durations.takePair(ev.SessionID, pairKey(*ev))
		if rec.StartedAt != "" {
			ev.StartedAt = rec.StartedAt
		}
		if ev.Span == nil || rec.OperationID == "" {
			break
		}
		// The inequality MUST be computed before the assignment below
		// overwrites the value it compares against -- compare, then assign.
		if rec.OperationID != ev.Span.OperationID {
			if ev.Metadata == nil {
				ev.Metadata = map[string]any{}
			}
			ev.Metadata["pair_recovered"] = true
		}
		ev.Span.OperationID = rec.OperationID
	case client.EventSessionEnded:
		e.Durations.ClearSession(ev.SessionID)
		e.Turns.ClearSession(ev.SessionID)
	}
}

// Flush drains the given session's own spooled events -- its head file, then
// its tail -- one attempt per event, plus one retry for a transient failure
// (Spool.DrainSession), until empty or ctx
// runs out. It is bounded by ctx (the caller caps session-end flush so
// teardown is never delayed unduly).
//
// It owns the session's debounce lock, moved here from the two adapter callers
// because nothing could hold the choreography while it was duplicated: a drain
// renames the file aside BEFORE delivering and Maybe returns silently on a fresh
// lock, so an append during a drain is invisible to both. 81 events sat that way.
// The session's own stripe lock (DrainSession, Block mode) is what now actually
// serializes concurrent drainers; the debounce lock's job is narrower -- keeping
// RealtimeTrigger from spawning a REDUNDANT flusher mid-drain.
func (e *Engine) Flush(ctx context.Context, sessionID string, em Emitter) (int, error) {
	return e.drainUntilEmpty(ctx, sessionID, e.emitFunc(em))
}

func (e *Engine) drainUntilEmpty(ctx context.Context, sessionID string, fn FlushFunc) (int, error) {
	spool := e.speaking()
	total := 0
	var err error
	// Its own scope so the release is DEFERRED: a panic would strand the lock.
	func() {
		defer e.Spool.ReleaseFlushLock(sessionID)
		for pass := 1; pass <= MaxDrainPasses; pass++ {
			// Per pass, not once: a drain outliving the debounce window would otherwise
			// let Maybe's stale-lock takeover spawn a competing flusher mid-drain.
			e.Spool.TouchFlushLock(sessionID)

			var n int
			n, err = spool.DrainSession(ctx, sessionID, fn, DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout})
			total += n
			if err != nil {
				return
			}
			remaining := e.Spool.PendingCount(sessionID)
			if remaining == 0 {
				return
			}
			if pass == MaxDrainPasses {
				e.logf("spool: %s: %d event(s) remain after %d drain passes; they stay spooled for the sweep",
					sessionID, remaining, MaxDrainPasses)
			}
		}
	}()

	// One pass AFTER the release closes the sliver; a double drain is harmless.
	if err == nil && e.Spool.PendingCount(sessionID) > 0 {
		n, lateErr := spool.DrainSession(ctx, sessionID, fn, DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout})
		total += n
		err = lateErr
	}
	return total, err
}

// FlushAll drains every spooled session, each in Try mode (FlushAll owns none
// of them, unlike Flush's own session).
func (e *Engine) FlushAll(ctx context.Context, em Emitter) (int, error) {
	return e.speaking().FlushAll(ctx, e.emitFunc(em))
}

// DrainSession is the exported single-session drain every later caller
// builds on (a gate's own escalation, a lane daemon's queue): sessionID's
// events through em, one attempt each plus one retry for a transient
// failure, per opts. See Spool.DrainSession for
// the full contract.
func (e *Engine) DrainSession(ctx context.Context, sessionID string, em Emitter, opts DrainOptions) (int, error) {
	return e.speaking().DrainSession(ctx, sessionID, e.emitFunc(em), opts)
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
	if ctx.Err() != nil || errors.Is(err, errPassCutShort) {
		// The walk stopped partway, so files behind it were never opened -- and a
		// stale mtime on one of those means nobody TRIED for the retention age, not
		// that the age was spent failing to deliver. A flusher that was simply down
		// for a month leaves exactly that, and its first pass back would delete
		// still-deliverable evidence without ever attempting it. errPassCutShort is
		// the same story one layer down: DrainSession refused to even rotate a stem
		// once its remaining budget could not cover one attempt, so that stem (and
		// everything walked after it) is exactly as untried as one ctx never reached.
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
// blocking. It is Deliver with the error kept, because the spool's drain loop
// (unlike a lane daemon's bounded pool) needs it to decide whether a file
// stays spooled for the next pass.
func (e *Engine) emitFunc(em Emitter) FlushFunc {
	logger := log.New(logfWriter{logf: e.logf}, "", 0)
	return func(ctx context.Context, ev client.DevEvent) error {
		_, err := Deliver(ctx, em, e.Advisory, ev, logger)
		return err
	}
}

// Deliver is the one delivery body every path in this repo uses to get an
// event to core and record its verdict: client.Emit, then Advisory.Record,
// then the cross-lane HALT latch. The hook flusher
// calls it through emitFunc above; the two in-process lane daemons
// (telemetry.go, transport.go) call it directly from a bounded pool, never
// the request goroutine. advisory may be nil (still delivers, records
// nothing). logger may be nil; the latch write substitutes a discard logger
// rather than requiring every caller to build one just to satisfy
// WriteSessionHalt's own non-nil contract.
//
// The latch condition is deliberately `err == nil && eval.Verdict ==
// client.VerdictHalt`: em.Emit returns the zero Evaluation on a transport
// fault (client.Client.Emit's own contract), so a HALT can never be a
// misread error, and an error from core is never latched.
//
// This is the ONLY thing keeping a halted developer session refused on the
// relay: the OpenBox core server's own halted-session pre-check skips
// developer sessions, so it never fires for the sessions this repo
// governs. Removing this write silently un-halts every halted run on every
// lane but the hook path's own narrow prompt-contract write (gate.go).
func Deliver(ctx context.Context, em Emitter, advisory *Advisory, ev client.DevEvent, logger *log.Logger) (client.Evaluation, error) {
	start := time.Now()
	traceDeliverAttempt(ev, deliveryAttempt(ctx))
	eval, err := em.Emit(ctx, ev)
	traceDeliverResult(ev, deliveryAttempt(ctx), eval, err, time.Since(start))
	// On success, a real verdict is recorded. Either way this cannot block the
	// tool call.
	if advisory != nil {
		advisory.Record(ev, eval)
	}
	if err == nil && eval.Verdict == client.VerdictHalt {
		WriteSessionHalt(loggerOrDiscard(logger), runIDFor(ev), eval)
	}
	return eval, err
}

// discardLogger is what Deliver substitutes for a nil *log.Logger:
// WriteSessionHalt itself requires a non-nil one, and a caller indifferent to
// the latch's own diagnostics (a test, a pool built without one) must not
// have to build a real one just to avoid a nil-pointer panic three calls
// deep.
var discardLogger = log.New(io.Discard, "", 0)

func loggerOrDiscard(logger *log.Logger) *log.Logger {
	if logger != nil {
		return logger
	}
	return discardLogger
}

// runIDFor mirrors client's own unexported runIDFor selection (RunID when a
// producer already stamped one from its own run record, else SessionID at
// generation 0) so the latch keys on the SAME run a wire payload names.
// It cannot call client.runIDFor directly (unexported, package client) or
// resolve session->run itself via git.RunStore: internal/adapters/common/git
// imports this package for AtomicWriteFile, so the reverse import would
// cycle. This is not a gap: every producer that reaches Deliver --
// claude-code's mapper (mapper.go:112, stamped from hookrun.go's own
// RunStore.Read at hook time), gatewayemit's Emitter (resolveRun) and
// telemetryemit's Mapper (turnFor) -- already resolves session->run via ITS
// OWN RunStore.Read and stamps ev.RunID before the event ever reaches
// Deliver, so this selection reads back a value already correctly resolved
// upstream rather than recomputing it.
func runIDFor(ev client.DevEvent) string {
	if ev.RunID != "" {
		return ev.RunID
	}
	return ev.SessionID
}

// logfWriter adapts a Printf-shaped logf (Engine's own e.logf, itself
// "Nil ⇒ silent") to an io.Writer, so Deliver's latch write -- which needs a
// *log.Logger for WriteSessionHalt -- can share Engine's own destination
// without a second "Nil ⇒ silent" rule ever drifting from e.logf's.
type logfWriter struct {
	logf func(format string, args ...any)
}

func (w logfWriter) Write(p []byte) (int, error) {
	if w.logf != nil {
		w.logf("%s", strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}
