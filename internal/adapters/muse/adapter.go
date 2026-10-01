package muse

import (
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// Emitter is the transport the adapter delivers events through; satisfied by
// *client.Client. Aliased from hookflow so callers of this package do not need
// to import it just to name the seam.
type Emitter = hookflow.Emitter

// Adapter is the Muse realization of the Provider Adapter Contract: this
// package's Mapper over the shared hookflow.Engine.
type Adapter = hookflow.Adapter[HookName, HookEvent, Mapper]

// New builds an Adapter for a developer identity, spooling under spoolDir and
// writing Advisory records to the default sink.
func New(id Identity, spoolDir string) *Adapter {
	return hookflow.NewAdapter[HookName, HookEvent](NewMapper(id), spoolDir)
}
