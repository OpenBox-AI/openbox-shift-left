package gatewayservice

import (
	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
)

const (
	LaunchdLabel    = laneservice.GatewayLabel
	SystemdUnitName = laneservice.GatewaySystemdName + ".service"
)

// StopTimeout is the supervisor's stop timeout, and it must match the
// gateway's --shutdown-grace. See laneservice.StopTimeout for the reasoning;
// it is aliased rather than restated so the two cannot drift apart.
const StopTimeout = laneservice.StopTimeout

const verboseFlag = laneservice.VerboseFlag

// spec carries OPENBOX_SESSION_DIR (phase 08, insight 8): unlike the
// telemetry/transport lanes, the gateway unit carried NO env block at all
// before this, yet gatewayemit (the :gateway: and :proxy: lanes' emitter)
// needs the same resolved directory the hooks use, or its lane disagrees with
// them about which run a call belongs to. Resolved here rather than passed
// in: a daemon has no $HOME, so the value has to be the ANSWER, not a
// passthrough (cmd/openbox/initlane.go's laneUnitEnv states the same rule).
func spec(addr, upstream, settingsPath string, verbose bool) laneservice.Spec {
	return laneservice.Gateway(addr, upstream, settingsPath, verbose).WithEnv(map[string]string{
		obgit.EnvSessionDir: obgit.DefaultSessionDir(),
	})
}

// probeSpec stays env-free: it exists to ADDRESS an already-installed unit
// (stop/uninstall/log paths), never to write one.
func probeSpec() laneservice.Spec {
	return laneservice.Gateway(DefaultProbeAddr, DefaultProbeUpstream, "", false)
}

// LaunchdPlist renders the macOS unit.
func LaunchdPlist(homeDir, binPath, addr, upstream string, verbose bool) string {
	return spec(addr, upstream, SettingsPath(homeDir), verbose).LaunchdPlist(homeDir, binPath)
}

// SystemdUnit renders the Linux user unit; homeDir is for the settings path.
func SystemdUnit(homeDir, binPath, addr, upstream string, verbose bool) string {
	return spec(addr, upstream, SettingsPath(homeDir), verbose).SystemdUnit(binPath)
}

// LogPath is where a supervised gateway's stdio is kept.
func LogPath(homeDir string) string { return probeSpec().LogPath(homeDir) }

// LaunchdPath is where the plist goes for a user-scope install.
func LaunchdPath(homeDir string) string { return probeSpec().LaunchdPath(homeDir) }

// SystemdPath is where the user unit goes.
func SystemdPath(homeDir string) string { return probeSpec().SystemdPath(homeDir) }

// UnitPath is where this OS's unit lives, or "" where none is packaged.
func UnitPath(goos, homeDir string) string { return probeSpec().UnitPath(goos, homeDir) }

// WriteUnit writes the unit for the given OS and returns its path.
func WriteUnit(goos, homeDir, binPath, addr, upstream string, verbose bool) (string, error) {
	return spec(addr, upstream, SettingsPath(homeDir), verbose).WriteUnit(goos, homeDir, binPath)
}

// RemoveUnit is the uninstall half.
func RemoveUnit(goos, homeDir string) (string, error) {
	return probeSpec().RemoveUnit(goos, homeDir)
}
