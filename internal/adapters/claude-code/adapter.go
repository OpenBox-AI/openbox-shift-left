package claudecode

import (
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// Emitter is the transport the adapter delivers events through; satisfied by
// *client.Client. Aliased from hookflow so callers of this package do not need
// to import it just to name the seam.
type Emitter = hookflow.Emitter

// Adapter is the Claude Code realization of the Provider Adapter Contract.
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
// it to the engine, which spools it. A payload that maps to nothing (missing
// session id / bad DID) is silently dropped.
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
