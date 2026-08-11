package hookflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"path/filepath"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// Emitter is the transport events are delivered through — satisfied by
// *client.Client. Abstracted so the flush path is unit-testable with a fake that
// records what would be emitted. It returns the rich Evaluation; observe-only
// callers still ignore it for control flow.
type Emitter interface {
	Emit(ctx context.Context, ev client.DevEvent) (client.Evaluation, error)
}

// Engine owns everything a hook run does with an event once it has been mapped:
// spool it, bridge its start time to the paired completion, deliver it, and
// record the advisory verdict. None of that is provider-specific — mapping a
// native hook payload onto a DevEvent is the only part that genuinely is, and it
// stays in the adapter.
//
// It holds no secrets: identity is a DID only, and the obx_ key and Ed25519 seed
// live in the Emitter's client (INV-1).
type Engine struct {
	Spool Spool
	// Advisory records the Advisory-tier verdict/guardrail signals on flush.
	// Record-only, off the hot path, never blocks (INV-3). nil ⇒ no advisory
	// recording (still delivers, still never blocks).
	Advisory *Advisory
	// Durations bridges the PreToolUse start time to the paired PostToolUse so a
	// completed span computes a real cross-process duration. Best-effort; an
	// empty Dir disables it.
	Durations DurationStash
}

// NewEngine builds an Engine spooling under dir and writing Advisory records to
// the default developer-scoped sink.
func NewEngine(spoolDir string) *Engine {
	return &Engine{
		Spool:    Spool{Dir: spoolDir},
		Advisory: &Advisory{Path: DefaultAdvisoryPath()},
		// A subdir of the spool root: keeps everything under one configurable dir
		// (OPENBOX_SPOOL_DIR → auto-isolated in tests), and the spool's FlushAll
		// skips subdirectories, so it never mistakes a stash for a spool file.
		Durations: DurationStash{Dir: filepath.Join(spoolDir, "durations")},
	}
}

// Record is the hot path: thread the tool call's duration and append the event
// to the local spool. It performs NO network I/O and NEVER blocks or fails a
// tool call (INV-3). The returned error is a local spool-write failure the
// caller logs fail-open — it is never surfaced to the developer tool.
func (e *Engine) Record(ev client.DevEvent) error {
	// Runs before Append so the spooled completed DevEvent is self-contained
	// (it survives spool splitting). Exported separately because pairing
	// behaviour is worth asserting on its own, without a spool write.
	e.ThreadDuration(&ev)
	return e.Spool.Append(ev)
}

// RecordDeferred is Record split at the point where delivery matters: it threads
// the duration NOW and returns the spool write as a closure, for a caller that
// does not yet know whether this same event is about to be delivered
// synchronously.
//
// The split is not arbitrary. The two halves have different dependencies:
//
//	duration stash — must be written before the tool runs, and says nothing
//	                 about delivery. Always runs.
//	spool append   — is a SECOND copy of the event once the Tier-2 escalation
//	                 has POSTed the identical one, because core does not dedupe
//	                 developer events on their id. Runs only if T2 did not.
//
// Threading the stash unconditionally is what keeps duration_ms working for
// escalated calls: the PostToolUse half recovers the start time from the stash,
// so suppressing the spool copy must not suppress the stash write with it.
func (e *Engine) RecordDeferred(ev client.DevEvent) func() error {
	e.ThreadDuration(&ev)
	return func() error { return e.Spool.Append(ev) }
}

// ThreadDuration records/recovers a tool call's start time across the separate
// Pre/PostToolUse hook processes (DurationStash), mutating only ev.StartedAt on
// the completed (ToolResult) event — which the client turns into the completed
// span's start_time, and thus a non-zero duration_ns. Structural only (INV-2)
// and best-effort (INV-3): a stash fault only costs duration accuracy. The
// session-ended event sweeps the session's stash so records from tool calls
// whose completion never fired do not accumulate.
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
	}
}

// Flush drains the given session's spooled events through the Emitter, then
// sweeps carry-over (recovery) files left undelivered by earlier flushes. It is
// bounded by ctx (the caller caps session-end flush so teardown is never delayed
// unduly). Observe-only: the verdict from each Emit never blocks; it is recorded
// to the Advisory sink off the hot path.
//
// The sweep is not limited to this session: a recovery file belongs to a session
// that has already ended, so it has no trigger of its own, and session end is the
// only ambient point where retry can happen at all (E8-S7). This session's own
// spool is drained first, and its own carry-over before other sessions', so the
// ctx budget goes to the ending session's telemetry before anyone's backlog.
// Both counts are returned as one total.
func (e *Engine) Flush(ctx context.Context, sessionID string, em Emitter) (int, error) {
	fn := e.emitFunc(em)
	// Snapshot the carry-over set BEFORE draining, so files this flush itself
	// writes are left for the next session rather than consuming their whole
	// retry allowance in one pass.
	carried := e.Spool.recoveryFiles()
	n, err := e.Spool.FlushSession(ctx, sessionID, fn)
	swept, sweepErr := e.Spool.sweepRecovery(ctx, carried, sessionID, fn)
	if err == nil {
		err = sweepErr
	}
	return n + swept, err
}

// FlushAll drains every spooled session (the `flush` subcommand / CLI catch-up).
func (e *Engine) FlushAll(ctx context.Context, em Emitter) (int, error) {
	return e.Spool.FlushAll(ctx, e.emitFunc(em))
}

// emitFunc adapts an Emitter to a FlushFunc. It emits the event and then records
// the returned Evaluation to the Advisory sink — record-only, never blocking.
// The delivery error is returned for the drain's fail-open logging; the advisory
// write itself never fails the drain.
func (e *Engine) emitFunc(em Emitter) FlushFunc {
	return func(ctx context.Context, ev client.DevEvent) error {
		eval, err := em.Emit(ctx, ev)
		// Record even alongside a delivery error: Emit is fail-open, so eval is
		// then the zero Evaluation (IsAdvisory false ⇒ nothing recorded). On
		// success, a real verdict is recorded. Either way this cannot block the
		// tool call.
		e.Advisory.Record(ev, eval)
		return err
	}
}

// randomID mints a filename suffix for spool rotation and orphan reclaim.
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Non-fatal: fall back to a fixed marker (a crypto/rand failure is
		// astronomically unlikely); a colliding suffix only risks losing the race
		// for one orphaned spool file, never a tool call.
		return "sfx-fallback"
	}
	return hex.EncodeToString(b[:])
}
