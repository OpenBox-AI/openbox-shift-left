package claudecode

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

// RunRewake is the optional background approval watcher (E9 §2.2).
// Unlike RunHook it reports an exit code, because that is how Claude Code's
// `asyncRewake` handler learns there is something to tell the session.
func (Engine) RunRewake(stdin io.Reader, wake io.Writer, logger *log.Logger) int {
	return RunRewake(stdin, wake, logger)
}

var (
	_ providerspi.HookEngine = Engine{}
	_ providerspi.Rewaker    = Engine{}
)
