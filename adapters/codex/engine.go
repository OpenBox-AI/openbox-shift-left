package codex

import (
	"io"
	"log"

	providerspi "github.com/openbox-ai/openbox-shift-left/provider"
)

// Engine is this adapter's runtime half of the provider SPI. RunHook and
// Capabilities already had these signatures as package functions; Engine names
// them as the contract so the CLI can dispatch through the registry instead of
// a hard-coded switch.
type Engine struct{}

// RunHook handles one native hook event. It never fails a tool call (INV-3):
// faults are recovered and logged, and the caller always exits 0.
func (Engine) RunHook(event string, stdin io.Reader, stdout io.Writer, logger *log.Logger) {
	RunHook(event, stdin, stdout, logger)
}

// Capabilities declares what this adapter supports.
func (Engine) Capabilities() []providerspi.Capability { return Capabilities() }

var _ providerspi.HookEngine = Engine{}
