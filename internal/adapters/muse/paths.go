package muse

import (
	"os"
	"path/filepath"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// spoolSubdir is this adapter's session spool subdirectory; cmd/openbox's
// providerSpoolSubdir names the same literal for the lane daemons.
const spoolSubdir = "muse-spool"

// DefaultSpoolDir is where this adapter spools events before flush.
func DefaultSpoolDir() string { return devconfig.SpoolDir(spoolSubdir) }

// SettingsPath is Muse's user-wide settings file, which carries its hooks
// (`~/.config/muse/settings.json`, schema_version 1).
func SettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".config", "muse", "settings.json")
}

// BakedHome is the directory the installer bakes into every handler as
// `--home`, or "" when there is nothing to bake. Muse clears the environment a
// hook runs in, so OPENBOX_HOME never reaches it; without the argument a hook
// would bind the default home and govern nothing on a machine that keeps its
// identity elsewhere. The default home needs no argument, because the hook
// derives it from HOME, which Muse passes through.
func BakedHome() string {
	if os.Getenv(devconfig.EnvHome) == "" {
		return ""
	}
	home, err := devconfig.Home()
	if err != nil {
		return ""
	}
	userHome, err := os.UserHomeDir()
	if err == nil && home == filepath.Join(userHome, ".openbox") {
		return ""
	}
	return home
}
