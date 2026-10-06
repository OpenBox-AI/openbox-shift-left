package hookflow

import (
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// openboxConfigDir the sinks in this package resolve their base through
// devconfig, which owns it, rather than through a second copy of the same
// fallback -- two copies is how the enforcement sink and the spool end up on
// different bases.
func openboxConfigDir() string {
	return devconfig.ConfigDir()
}

// EnvStaleDir named the stale-marker directory the session-start freshness
// check wrote to. Deliberately not re-pointed at anything: there is no bundle
// path to resolve any more, so ResolveBundlePath was deleted rather than made
// to return a plausible-looking value nothing reads.
const EnvStaleDir = "OPENBOX_STALE_DIR"
