package gatewayservice

import (
	"github.com/kardianos/service"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
)

// service.go is the gateway's view of the supervisor lifecycle (D-OSS-3).
//
// The library/custom boundary, the reason both unit bodies are supplied as
// template overrides, and why Start/Stop stay with the caller are all documented
// once in cli/internal/laneservice — restating them per lane is how three copies
// of one decision come to disagree. This file exists so the gateway's call sites,
// doctor and its tests keep the names they had.

// serviceName is the library's `Name` for this lane, per platform.
//
// It is no longer on any production path — New/Install/Reinstall/Uninstall all
// go through the lane spec — and is kept because a test asserts that the path the
// library derives from it is exactly the path this repo already uses on both
// platforms — the two conventions differ (reverse-DNS label vs hyphenated
// unit), and one shared value would silently rename one of them.
func serviceName(goos string) (string, error) {
	switch goos {
	case "darwin":
		return LaunchdLabel, nil
	case "linux":
		return laneservice.GatewaySystemdName, nil
	default:
		return "", probeSpec().UnsupportedPlatform(goos)
	}
}

// New returns the supervisor handle for the gateway on this platform.
func New(goos, homeDir, binPath, addr, upstream string, verbose bool) (service.Service, error) {
	return spec(addr, upstream, verbose).New(goos, homeDir, binPath)
}

// Install writes the unit through the library. It does NOT start it: keeping the
// write separate from the load is what makes "failed to configure" and "failed to
// start" distinguishable, the same reason doctor separates alive from actually
// used.
func Install(goos, homeDir, binPath, addr, upstream string, verbose bool) error {
	return spec(addr, upstream, verbose).Install(goos, homeDir, binPath)
}

// Reinstall writes the unit, replacing one that is already there — the path by
// which a unit written by an OLDER binary gets refreshed.
func Reinstall(goos, homeDir, binPath, addr, upstream string, verbose bool) error {
	return spec(addr, upstream, verbose).Reinstall(goos, homeDir, binPath)
}

// Uninstall removes the unit. Absent is success: `--remove-gateway` must be safe
// on a machine that never had one.
func Uninstall(goos, homeDir string) error {
	return probeSpec().Uninstall(goos, homeDir)
}

// DefaultProbeAddr / DefaultProbeUpstream are placeholders for the uninstall
// path, which needs a Spec to address the unit but no working argv — nothing is
// rendered or started. They are named rather than inlined so a reader does not
// mistake them for a default the install path uses.
const (
	DefaultProbeAddr     = "127.0.0.1:8788"
	DefaultProbeUpstream = "https://api.anthropic.com"
)

// IsNotInstalled reports whether an Uninstall error means there was nothing to
// remove.
func IsNotInstalled(err error) bool { return laneservice.IsNotInstalled(err) }
