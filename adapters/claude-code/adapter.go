package claudecode

import (
	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
)

// Emitter is the transport the adapter delivers events through — satisfied by
// *client.Client. Aliased from hookflow so callers of this package do not need
// to import it just to name the seam.
type Emitter = hookflow.Emitter

// Adapter is the Claude Code realization of the Provider Adapter Contract.
//
// Mapping a native hook payload onto a normalized DevEvent is the only part of
// a hook run that is genuinely Claude-Code-specific; everything after it — the
// durable spool, the duration stash, delivery, the advisory sink — is shared
// with every other provider and lives in hookflow. So the adapter is a Mapper
// plus that engine.
type Adapter struct {
	Mapper Mapper
	*hookflow.Engine
}

// New builds an Adapter for a developer identity, spooling under dir and
// writing Advisory records to the default sink.
func New(id Identity, spoolDir string) *Adapter {
	return &Adapter{
		Mapper: NewMapper(id),
		Engine: hookflow.NewEngine(spoolDir),
	}
}

// Observe is the hot path: map one hook payload to a normalized event and hand
// it to the engine, which spools it. It performs NO network I/O and NEVER
// blocks or fails a tool call (INV-3). A payload that maps to nothing (missing
// session id / bad DID) is silently dropped. The returned bool reports whether
// an event was spooled (for the binary's diagnostics only); the returned error
// is a local spool-write failure the caller logs fail-open — it is never
// surfaced to Claude Code.
func (a *Adapter) Observe(hook HookName, e *HookEvent) (spooled bool, err error) {
	ev, ok := a.Mapper.Map(hook, e)
	if !ok {
		return false, nil
	}
	if err := a.Record(ev); err != nil {
		return false, err
	}
	return true, nil
}
