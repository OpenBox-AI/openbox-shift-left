// Package codex is the OpenAI Codex CLI realization of the Provider Adapter
// Contract; the observe leg. It maps Codex's native hooks (v0.145.0+, hooks
// stable and on by default) onto the normalized developer event contract and
// emits them through the shared AIP-signed transport on the flat hook wire.
// Observe-only, fail-open; it never blocks, denies, or slows a Codex tool call
// (INV-3; the enforce leg is in enforce.go).
package codex

import (
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// Emitter is the transport the adapter delivers events through; satisfied by
// *client.Client. Aliased from hookflow so callers of this package do not need
// to import it just to name the seam.
type Emitter = hookflow.Emitter

// Adapter is the Codex realization of the Provider Adapter Contract.
type Adapter struct {
	Mapper Mapper
	*hookflow.Engine
}

// New builds an Adapter for a developer identity, spooling under dir and
// writing Advisory records to the default sink; developer-scoped, not tool-
// scoped, so it is deliberately shared with the other adapters.
func New(id Identity, spoolDir string) *Adapter {
	return &Adapter{
		Mapper: NewMapper(id),
		Engine: hookflow.NewEngine(spoolDir),
	}
}

// Observe is the hot path: map one hook payload to a normalized event and hand
// it to the engine, which threads the tool call's duration and spools it. A
// payload that maps to nothing (missing session id / bad DID) is silently
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
