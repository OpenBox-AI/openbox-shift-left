package gatewayservice

import (
	"github.com/kardianos/service"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
)

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

// Install writes the unit through the library.
func Install(goos, homeDir, binPath, addr, upstream string, verbose bool) error {
	return spec(addr, upstream, verbose).Install(goos, homeDir, binPath)
}

// Reinstall writes the unit, replacing one that is already there; the path by
// which a unit written by an older binary gets refreshed.
func Reinstall(goos, homeDir, binPath, addr, upstream string, verbose bool) error {
	return spec(addr, upstream, verbose).Reinstall(goos, homeDir, binPath)
}

// Uninstall removes the unit. Absent is success: `--remove-gateway` must be
// safe on a machine that never had one.
func Uninstall(goos, homeDir string) error {
	return probeSpec().Uninstall(goos, homeDir)
}

const (
	DefaultProbeAddr     = "127.0.0.1:8788"
	DefaultProbeUpstream = "https://api.anthropic.com"
)

// IsNotInstalled reports whether an Uninstall error means there was nothing to
// remove.
func IsNotInstalled(err error) bool { return laneservice.IsNotInstalled(err) }
