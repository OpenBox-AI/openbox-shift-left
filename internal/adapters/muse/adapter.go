package muse

import (
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// Emitter is the transport the adapter delivers events through; satisfied by
// *client.Client. Aliased from hookflow so callers of this package do not need
// to import it just to name the seam.
type Emitter = hookflow.Emitter

// Adapter is the Muse realization of the Provider Adapter Contract.
type Adapter struct {
	Mapper Mapper
	*hookflow.Engine
}

// New builds an Adapter for a developer identity, spooling under dir.
func New(id Identity, spoolDir string) *Adapter {
	return &Adapter{
		Mapper: NewMapper(id),
		Engine: hookflow.NewEngine(spoolDir),
	}
}

// Observe maps one hook payload to a normalized event and hands it to the
// engine, which threads the tool call's duration and spools it. A payload that
// maps to nothing (no session id, bad DID, an event with no contract type) is
// dropped.
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
