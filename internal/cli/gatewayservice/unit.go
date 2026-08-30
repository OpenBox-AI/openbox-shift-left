package gatewayservice

import (
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
)

// unit.go is the gateway's view of the supervisor unit.
//
// The rendering, the paths, the write/remove pair and the escaping moved to
// internal/cli/laneservice when telemetry and transport needed the same
// mechanics. Nothing about the gateway's unit changed: this file names its Spec
// and keeps the call sites, doctor and the tests reading exactly as before.
//
// Why a shared implementation rather than a third copy: the unit body is where
// drift is least visible and most expensive. A lane whose plist forgot
// StandardErrorPath logs nowhere, and one whose ExitTimeOut stopped matching its
// own --shutdown-grace is SIGKILLed mid-drain on every routine restart. Neither
// surfaces as an error.

// Label / unit names. Fixed strings, because the uninstall path has to find
// exactly what the install path wrote — and sourced from the lane spec rather
// than re-typed. They agreed by inspection
// before, which is the duplicated-literal shape this repo keeps paying for.
const (
	LaunchdLabel    = laneservice.GatewayLabel
	SystemdUnitName = laneservice.GatewaySystemdName + ".service"
)

// StopTimeout is the supervisor's stop timeout, and it MUST match the gateway's
// --shutdown-grace. See laneservice.StopTimeout for the reasoning; it is aliased
// rather than restated so the two cannot drift apart.
const StopTimeout = laneservice.StopTimeout

// verboseFlag is the one spelling of the flag, shared with the other lanes.
const verboseFlag = laneservice.VerboseFlag

// spec is the gateway's lane descriptor.
func spec(addr, upstream string, verbose bool) laneservice.Spec {
	return laneservice.Gateway(addr, upstream, verbose)
}

// probeSpec addresses the unit for paths and removal, where no working argv is
// needed because nothing is rendered or started.
func probeSpec() laneservice.Spec {
	return laneservice.Gateway(DefaultProbeAddr, DefaultProbeUpstream, false)
}

// LaunchdPlist renders the macOS unit.
func LaunchdPlist(homeDir, binPath, addr, upstream string, verbose bool) string {
	return spec(addr, upstream, verbose).LaunchdPlist(homeDir, binPath)
}

// SystemdUnit renders the Linux USER unit.
func SystemdUnit(binPath, addr, upstream string, verbose bool) string {
	return spec(addr, upstream, verbose).SystemdUnit(binPath)
}

// LogPath is where a supervised gateway's stdio is kept.
//
// Under the OpenBox config dir rather than ~/Library/Logs: it is OpenBox state,
// `openbox doctor` can name one path on both platforms, and the directory
// already exists with the right permissions.
func LogPath(homeDir string) string { return probeSpec().LogPath(homeDir) }

// LaunchdPath is where the plist goes for a user-scope install.
func LaunchdPath(homeDir string) string { return probeSpec().LaunchdPath(homeDir) }

// SystemdPath is where the user unit goes.
func SystemdPath(homeDir string) string { return probeSpec().SystemdPath(homeDir) }

// UnitPath is where this OS's unit lives, or "" where none is packaged. Exposed
// so an uninstall can unload the job BEFORE deleting the file the unload needs.
func UnitPath(goos, homeDir string) string { return probeSpec().UnitPath(goos, homeDir) }

// WriteUnit writes the unit for the given OS and returns its path.
//
// NOT THE PRODUCTION PATH since D-OSS-3 — Reinstall is, and it goes through
// kardianos/service. This survives because it is the only way to assert the
// WRITTEN ARTIFACT at a caller-chosen location; see laneservice.Spec.WriteUnit.
func WriteUnit(goos, homeDir, binPath, addr, upstream string, verbose bool) (string, error) {
	return spec(addr, upstream, verbose).WriteUnit(goos, homeDir, binPath)
}

// RemoveUnit is the uninstall half.
func RemoveUnit(goos, homeDir string) (string, error) {
	return probeSpec().RemoveUnit(goos, homeDir)
}
