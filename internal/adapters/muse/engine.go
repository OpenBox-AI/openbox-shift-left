package muse

import (
	"io"
	"log"
	"time"

	providerspi "github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// FaultExitCode is what a gated Muse hook exits with when it cannot produce an
// answer. Muse reads exit 0 with no usable answer as allow, and treats exit 2 as
// a block on a blocking event; any other non-zero exit is a failed hook, which
// starts the deny-only onFailure successor. 2 is the one code that denies on
// both readings, so a crash is never an allow.
const FaultExitCode = 2

// Engine is the Muse adapter's runtime hook engine.
type Engine struct{}

// RunHook handles one native Muse hook event. On a gated event it does not
// recover a panic: the panic reaches `openbox hook`'s own recover, which exits
// with FaultExitCode. Swallowing it here would exit 0, which Muse reads as an
// allow.
func (Engine) RunHook(event string, stdin io.Reader, stdout io.Writer, logger *log.Logger) {
	RunHook(event, stdin, stdout, logger)
}

// FaultExitCode reports the exit code a crashed hook must return: FaultExitCode
// for a gated event, 0 where a crash gates nothing.
func (Engine) FaultExitCode(event string) int {
	if HookName(event).Gated() {
		return FaultExitCode
	}
	return 0
}

// Capabilities declares what this adapter supports.
func (Engine) Capabilities() []providerspi.Capability { return Capabilities() }

// HookCeilings are the limits the handlers this adapter installs declare: 30s on
// a gated event, 5s on every other.
func (Engine) HookCeilings() providerspi.HookCeiling {
	return providerspi.HookCeiling{Gating: 30 * time.Second, Other: 5 * time.Second}
}

var (
	_ providerspi.HookEngine  = Engine{}
	_ providerspi.FaultExiter = Engine{}
)
