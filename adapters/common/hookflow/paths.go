package hookflow

import (
	"os"

	"github.com/openbox-ai/openbox-shift-left/decision"
)

// Env overrides the engine honours directly. They are engine concerns rather
// than part of the shared dev.json contract, so they live here rather than in
// devconfig.
const (
	// EnvStaleDir relocates the stale-marker directory (tests isolate it).
	EnvStaleDir = "OPENBOX_STALE_DIR"
	// EnvBundle overrides the local decision bundle path; enforce-only.
	EnvBundle = "OPENBOX_SIDECAR_BUNDLE"
)

// ResolveBundlePath resolves the local policy bundle the decider loads.
func ResolveBundlePath() string {
	if p := os.Getenv(EnvBundle); p != "" {
		return p
	}
	return decision.DefaultBundlePath()
}
