package claudecode

import (
	"context"
	"path/filepath"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// Emitter is the transport the adapter delivers events through — satisfied
// by *client.Client. Abstracted so the flush path is unit-testable with a
// fake that records what would be emitted. It returns the rich Evaluation;
// observe-only callers still ignore it for control flow.
type Emitter interface {
	Emit(ctx context.Context, ev client.DevEvent) (client.Evaluation, error)
}

// Adapter is the Claude Code realization of the Provider Adapter Contract.
// It owns the hot-path mapping+spool (Observe) and the off-path delivery
// (Flush). It holds no secrets — identity is a DID only; the obx_ key +
// Ed25519 seed live in the Emitter's client (INV-1).
type Adapter struct {
	Mapper Mapper
	Spool  Spool
	// Advisory records the Advisory-tier verdict/guardrail signals on
	// flush. Record-only, off the hot path, never blocks (INV-3). nil ⇒ no
	// advisory recording (still delivers, still never blocks).
	Advisory *Advisory
	// Durations bridges the PreToolUse start time to the paired PostToolUse
	// so a completed span computes a real cross-process duration.
	// Best-effort; an empty Dir disables it (Observe threads nothing).
	Durations durationStash
}

// New builds an Adapter for a developer identity, spooling under dir and writing
// Advisory records to the default sink (DefaultAdvisoryPath).
func New(id Identity, spoolDir string) *Adapter {
	return &Adapter{
		Mapper:   NewMapper(id),
		Spool:    Spool{Dir: spoolDir},
		Advisory: &Advisory{Path: DefaultAdvisoryPath()},
		// A subdir of the spool root: keeps everything under one configurable dir
		// (OPENBOX_SPOOL_DIR → auto-isolated in tests), and the spool's FlushAll
		// skips subdirectories, so it never mistakes a stash for a spool file.
		Durations: durationStash{Dir: filepath.Join(spoolDir, "durations")},
	}
}

// Observe is the hot path: map one hook payload to a normalized event and append
// it to the local spool. It performs NO network I/O and NEVER blocks or fails a
// tool call (INV-3). A payload that maps to nothing (missing session id / bad
// DID) is silently dropped. The returned bool reports whether an event was
// spooled (for the binary's diagnostics only); the returned error is a local
// spool-write failure the caller logs fail-open — it is never surfaced to Claude
// Code.
func (a *Adapter) Observe(hook HookName, e *HookEvent) (spooled bool, err error) {
	ev, ok := a.Mapper.Map(hook, e)
	if !ok {
		return false, nil
	}
	// Bridge the PreToolUse start time to the paired PostToolUse so the
	// completed span computes a real cross-process duration. Runs before
	// Append so the spooled completed DevEvent is self-contained (survives
	// spool splitting).
	a.threadDuration(&ev)
	if err := a.Spool.Append(ev); err != nil {
		return false, err
	}
	return true, nil
}

// threadDuration records/recovers a tool call's start time across the separate
// Pre/PostToolUse hook processes (durationStash), mutating only ev.StartedAt on
// the completed (ToolResult) event — which buildHookSpan turns into the completed
// span's start_time (and thus a non-zero duration_ns). Structural only (INV-2)
// and best-effort (INV-3): a stash fault only costs duration accuracy. SessionEnd
// sweeps the session's stash so records from tool calls whose PostToolUse never
// fired do not accumulate.
func (a *Adapter) threadDuration(ev *client.DevEvent) {
	switch ev.EventType {
	case client.EventToolCall:
		_ = a.Durations.putStart(ev.SessionID, toolCallStartKey(*ev), ev.StartedAt)
	case client.EventToolResult:
		if start := a.Durations.takeStart(ev.SessionID, toolCallStartKey(*ev)); start != "" {
			ev.StartedAt = start
		}
	case client.EventSessionEnded:
		a.Durations.clearSession(ev.SessionID)
	}
}

// Flush drains the given session's spooled events through the Emitter, then
// sweeps carry-over (recovery) files left undelivered by earlier flushes. It
// is bounded by ctx (the binary caps SessionEnd flush so session teardown
// is never delayed unduly). Observe-only: the verdict from each Emit never
// blocks; it is recorded to the Advisory sink off the hot path.
//
// The sweep is not limited to this session: a recovery file belongs to a session
// that has already ended, so it has no trigger of its own, and SessionEnd is the
// only ambient point where retry can happen at all (E8-S7). This session's own
// spool is drained first, and its own carry-over before other sessions', so the
// ctx budget goes to the ending session's telemetry before anyone's backlog.
// Both counts are returned as one total.
func (a *Adapter) Flush(ctx context.Context, sessionID string, em Emitter) (int, error) {
	fn := a.emitFunc(em)
	// Snapshot the carry-over set BEFORE draining, so files this flush itself
	// writes are left for the next session rather than consuming their whole
	// retry allowance in one pass.
	carried := a.Spool.recoveryFiles()
	n, err := a.Spool.FlushSession(ctx, sessionID, fn)
	swept, sweepErr := a.Spool.sweepRecovery(ctx, carried, sessionID, fn)
	if err == nil {
		err = sweepErr
	}
	return n + swept, err
}

// FlushAll drains every spooled session (the `flush` subcommand / CLI catch-up).
func (a *Adapter) FlushAll(ctx context.Context, em Emitter) (int, error) {
	return a.Spool.FlushAll(ctx, a.emitFunc(em))
}

// emitFunc adapts an Emitter to a FlushFunc. It emits the event and then
// records the returned Evaluation to the Advisory sink — record-only,
// never blocking. The delivery error is returned for the drain's fail-open
// logging; the advisory write itself never fails the drain.
func (a *Adapter) emitFunc(em Emitter) FlushFunc {
	return func(ctx context.Context, ev client.DevEvent) error {
		eval, err := em.Emit(ctx, ev)
		// Record even alongside a delivery error: Emit is fail-open, so err here
		// is a caller precondition (unbuildable event), and eval is then the zero
		// Evaluation (IsAdvisory false ⇒ nothing recorded). On success, a real
		// verdict is recorded. Either way this cannot block the tool call.
		a.Advisory.Record(ev, eval)
		return err
	}
}
