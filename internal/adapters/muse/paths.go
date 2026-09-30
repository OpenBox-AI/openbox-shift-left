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
