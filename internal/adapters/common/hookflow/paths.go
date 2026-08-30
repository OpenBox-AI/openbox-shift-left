package hookflow

import (
	"os"
	"path/filepath"
)

// openboxConfigDir is the base directory for the runtime's local governance
// state; the enforcement/advisory sinks and the session-halt latches: the
// platform user-config dir, with the classic ~/.config fallback when it cannot
// be resolved.
func openboxConfigDir() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "openbox")
}

// EnvStaleDir named the stale-marker directory the session-start freshness
// check wrote to. Deliberately not re-pointed at anything: there is no bundle
// path to resolve any more, so ResolveBundlePath was deleted rather than made
// to return a plausible-looking value nothing reads.
const EnvStaleDir = "OPENBOX_STALE_DIR"
