package muse

import (
	"io"
	"log"
	"time"

	providerspi "github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// Engine is the Muse adapter's runtime hook engine.
type Engine struct{}

// RunHook handles one native Muse hook event.
func (Engine) RunHook(event string, stdin io.Reader, stdout io.Writer, logger *log.Logger) {
	_, _ = io.Copy(io.Discard, stdin)
}

// Capabilities declares what this adapter supports.
func (Engine) Capabilities() []providerspi.Capability { return nil }

// HookCeilings are the limits the handlers this adapter installs declare:
// 30s on a gated event, 5s on every other.
func (Engine) HookCeilings() providerspi.HookCeiling {
	return providerspi.HookCeiling{Gating: 30 * time.Second, Other: 5 * time.Second}
}

var _ providerspi.HookEngine = Engine{}
