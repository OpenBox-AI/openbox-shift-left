package claudecode

import (
	"context"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// Emitter is the transport the adapter delivers events through — satisfied by
// *client.Client (STORY-SL-3). Abstracted so the flush path is unit-testable
// with a fake that records what would be emitted.
type Emitter interface {
	Emit(ctx context.Context, ev client.DevEvent) (client.Verdict, error)
}

// Adapter is the Claude Code realization of the Provider Adapter Contract
// (§1b). It owns the hot-path mapping+spool (Observe) and the off-path delivery
// (Flush). It holds no secrets — identity is a DID only; the obx_ key + Ed25519
// seed live in the Emitter's client (INV-1).
type Adapter struct {
	Mapper Mapper
	Spool  Spool
}

// New builds an Adapter for a developer identity, spooling under dir.
func New(id Identity, spoolDir string) *Adapter {
	return &Adapter{Mapper: NewMapper(id), Spool: Spool{Dir: spoolDir}}
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
	if err := a.Spool.Append(ev); err != nil {
		return false, err
	}
	return true, nil
}

// Flush drains the given session's spooled events through the Emitter. It is
// bounded by ctx (the binary caps SessionEnd flush so session teardown is never
// delayed unduly). Observe-only: the verdict from each Emit is ignored (D7).
func (a *Adapter) Flush(ctx context.Context, sessionID string, em Emitter) (int, error) {
	return a.Spool.FlushSession(ctx, sessionID, emitFunc(em))
}

// FlushAll drains every spooled session (the `flush` subcommand / CLI catch-up).
func (a *Adapter) FlushAll(ctx context.Context, em Emitter) (int, error) {
	return a.Spool.FlushAll(ctx, emitFunc(em))
}

// emitFunc adapts an Emitter to a FlushFunc, discarding the (ignored) verdict.
func emitFunc(em Emitter) FlushFunc {
	return func(ctx context.Context, ev client.DevEvent) error {
		_, err := em.Emit(ctx, ev)
		return err
	}
}
