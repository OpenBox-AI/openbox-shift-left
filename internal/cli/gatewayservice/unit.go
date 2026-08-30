package gatewayservice

import (
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

func spec(addr, upstream string, verbose bool) laneservice.Spec {
	return laneservice.Gateway(addr, upstream, verbose)
}

func probeSpec() laneservice.Spec {
	return laneservice.Gateway(DefaultProbeAddr, DefaultProbeUpstream, false)
}

// LaunchdPlist renders the macOS unit.
func LaunchdPlist(homeDir, binPath, addr, upstream string, verbose bool) string {
	return spec(addr, upstream, verbose).LaunchdPlist(homeDir, binPath)
}

// SystemdUnit renders the Linux user unit.
func SystemdUnit(binPath, addr, upstream string, verbose bool) string {
	return spec(addr, upstream, verbose).SystemdUnit(binPath)
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
	return spec(addr, upstream, verbose).WriteUnit(goos, homeDir, binPath)
}

// RemoveUnit is the uninstall half.
func RemoveUnit(goos, homeDir string) (string, error) {
	return probeSpec().RemoveUnit(goos, homeDir)
}
