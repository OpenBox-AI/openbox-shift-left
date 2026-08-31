package hookflow

import (
	"context"
	"path/filepath"

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
func (e *Engine) Flush(ctx context.Context, sessionID string, em Emitter) (int, error) {
	fn := e.emitFunc(em)
	carried := e.Spool.recoveryFiles()
	n, err := e.Spool.FlushSession(ctx, sessionID, fn)
	swept, sweepErr := e.Spool.sweepRecovery(ctx, carried, sessionID, fn)
	if err == nil {
		err = sweepErr
	}
	return n + swept, err
}

// FlushAll drains every spooled session (the `flush` subcommand / CLI catch-
// up).
func (e *Engine) FlushAll(ctx context.Context, em Emitter) (int, error) {
	return e.Spool.FlushAll(ctx, e.emitFunc(em))
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
