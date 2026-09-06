package main

import (
	"fmt"
	"runtime"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

type laneInstall struct {
	label   string
	addr    string
	homeDir string
	laneIdentity
	installUnit   func() error
	uninstallUnit func() error
	activate      func() (replaced []string, err error)
	envNotSet     string
}

type laneIdentity struct {
	unitPath     string
	launchdLabel string
	systemdUnit  string
}

func identityOf(spec laneservice.Spec, homeDir string) laneIdentity {
	return laneIdentity{
		unitPath:     spec.UnitPath(runtime.GOOS, homeDir),
		launchdLabel: spec.Label,
		systemdUnit:  spec.SystemdUnitName(),
	}
}

func (a *app) setupLane(in laneInstall) error {
	// Checked before starting, because the readiness probe below cannot tell our
	// daemon from a stranger: it is a bare TCP connect.
	if occupied, who := portOccupied(in.addr); occupied {
		if !unitDescribesAddr(in.unitPath, in.addr) {
			return fmt.Errorf("%s is already in use%s; refusing to continue, because the readiness check "+
				"cannot tell our %s from whatever is listening. Stop it, or choose another address",
				in.addr, who, in.label)
		}
		fmt.Fprintf(a.stdout, "  replacing      the %s already installed at %s (%s)\n", in.label, in.addr, in.unitPath)
		a.unloadUnit(in.laneIdentity)
		if !waitForPortFreeFn(in.addr, gatewayStopTimeout) {
			return fmt.Errorf("the %s already running on %s did not stop within %s; nothing was changed. "+
				"Stop it by hand and re-run init", in.label, in.addr, gatewayStopTimeout)
		}
	}

	if err := in.installUnit(); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "  %-14s %s\n", in.label+" unit", in.unitPath)

	if err := a.loadUnit(in.laneIdentity); err != nil {
		a.rollbackLaneUnit(in)
		return fmt.Errorf("wrote %s but could not start it (%w); %s. Start it by hand with "+
			"`openbox %s`, then re-run init", in.unitPath, err, in.envNotSet, in.label)
	}

	if !waitForListenerFn(in.addr, gatewayReadyTimeout) {
		a.rollbackLaneUnit(in)
		return fmt.Errorf("the %s did not start listening on %s within %s; %s. Check the service logs, "+
			"or run `openbox %s` in the foreground to see why", in.label, in.addr, gatewayReadyTimeout, in.envNotSet, in.label)
	}
	fmt.Fprintf(a.stdout, "  %-14s listening on %s\n", in.label, in.addr)

	replaced, err := in.activate()
	if err != nil {
		a.rollbackLaneUnit(in)
		return err
	}
	for _, r := range replaced {
		fmt.Fprintf(a.stdout, "  replaced       %s\n", r)
	}
	return nil
}

func (a *app) rollbackLaneUnit(in laneInstall) {
	if in.unitPath == "" {
		return
	}
	a.unloadUnit(in.laneIdentity)
	if err := in.uninstallUnit(); err == nil {
		fmt.Fprintf(a.stdout, "  rolled back    removed %s\n", in.unitPath)
	}
}

type laneRemoval struct {
	label string
	laneIdentity
	deactivate    func() error
	uninstallUnit func() error
}

func (a *app) removeLane(in laneRemoval) error {
	if in.deactivate != nil {
		if err := in.deactivate(); err != nil {
			return err
		}
	}
	a.unloadUnit(in.laneIdentity)
	if err := in.uninstallUnit(); err != nil {
		return err
	}
	if in.unitPath != "" {
		fmt.Fprintf(a.stdout, "  removed        %s\n", in.unitPath)
	}
	return nil
}

// laneUnitEnv is the coordinate environment the unit has to carry, captured
// from the installing process. A supervisor starts a daemon with no
// environment of its own, so devconfig.Home() inside it resolves the real
// ~/.openbox while this process validated the path its own $OPENBOX_HOME
// named — and the transport CA check then refuses a certificate that is not
// where it looked.
//
// Three keys now, and only path coordinates: a unit file is world-readable,
// so the API key and the signing seed must never reach one. The first two are
// overrides this process's own environment may or may not carry, so they are
// copied only when set. The third, OPENBOX_SESSION_DIR, is different in kind:
// it is RESOLVED rather than copied, because a daemon has no $HOME at all --
// obgit.DefaultSessionDir()'s os.UserConfigDir() fallback would resolve
// differently, or not at all, inside it (phase 08 insight 7). Without it, the
// gateway/proxy lanes would read (or bump) a run record this process never
// wrote and disagree with the hooks about which run a call belongs to.
func (a *app) laneUnitEnv() map[string]string {
	env := make(map[string]string, 3)
	for _, key := range []string{devconfig.EnvHome, devconfig.EnvSpoolDir} {
		if v := a.getenv(key); v != "" {
			env[key] = v
		}
	}
	env[obgit.EnvSessionDir] = obgit.DefaultSessionDir()
	return env
}

var installLaneUnitFn = func(spec laneservice.Spec, goos, homeDir, binPath string) error {
	return spec.Reinstall(goos, homeDir, binPath)
}

var uninstallLaneUnitFn = func(spec laneservice.Spec, goos, homeDir string) error {
	return spec.Uninstall(goos, homeDir)
}

// setupTelemetry installs the local OTLP receiver and points the tool's own
// telemetry at it.
func (a *app) setupTelemetry(homeDir, addr string, verbose bool) error {
	// The settings path goes INTO the unit: only the install path knows the real home.
	spec := laneservice.Telemetry(addr, claudeSettingsPath(homeDir), verbose).WithEnv(a.laneUnitEnv())
	binPath, err := a.selfPath()
	if err != nil {
		return err
	}
	settings := claudeSettingsPath(homeDir)
	return a.setupLane(laneInstall{
		label:         "telemetry",
		addr:          addr,
		homeDir:       homeDir,
		laneIdentity:  identityOf(spec, homeDir),
		installUnit:   func() error { return installLaneUnitFn(spec, runtime.GOOS, homeDir, binPath) },
		uninstallUnit: func() error { return uninstallLaneUnitFn(spec, runtime.GOOS, homeDir) },
		envNotSet: "the telemetry env keys were NOT written, so the tool exports nothing and this lane " +
			"records nothing; everything else on this machine is unaffected",
		activate: func() ([]string, error) {
			keys := activation.TelemetryKeys(addr)
			res, err := activation.Activate(homeDir, settings, activation.LaneTelemetry, keys)
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(a.stdout, "  %-14s %d keys -> %s (user scope: %s)\n",
				"telemetry env", len(keys), "http://"+addr, settings)
			fmt.Fprintf(a.stdout, "                 this makes the tool EXPORT its own telemetry, including prompt and\n")
			fmt.Fprintf(a.stdout, "                 tool content, to that local receiver. What leaves this machine is\n")
			fmt.Fprintf(a.stdout, "                 still gated by the content_capture posture.\n")
			return res.Replaced, nil
		},
	})
}

func (a *app) removeTelemetry(homeDir string, force bool) error {
	spec := laneservice.Telemetry(telemetry.DefaultAddr, "", false)
	settings := claudeSettingsPath(homeDir)
	return a.removeLane(laneRemoval{
		label:         "telemetry",
		laneIdentity:  identityOf(spec, homeDir),
		uninstallUnit: func() error { return uninstallLaneUnitFn(spec, runtime.GOOS, homeDir) },
		deactivate: func() error {
			return a.reportDeactivation("telemetry", homeDir, settings, activation.LaneTelemetry, force)
		},
	})
}

func (a *app) setupTransport(homeDir, addr string, verbose bool) error {
	spec := laneservice.Transport(addr, claudeSettingsPath(homeDir), verbose).WithEnv(a.laneUnitEnv())
	binPath, err := a.selfPath()
	if err != nil {
		return err
	}
	openboxHome, err := devconfig.Home()
	if err != nil {
		return err
	}
	caPath, _ := transport.CAPaths(openboxHome)
	settings := claudeSettingsPath(homeDir)

	return a.setupLane(laneInstall{
		label:         "transport",
		addr:          addr,
		homeDir:       homeDir,
		laneIdentity:  identityOf(spec, homeDir),
		installUnit:   func() error { return installLaneUnitFn(spec, runtime.GOOS, homeDir, binPath) },
		uninstallUnit: func() error { return uninstallLaneUnitFn(spec, runtime.GOOS, homeDir) },
		envNotSet: "the proxy env keys were NOT written, so model calls still reach the provider " +
			"directly and are unobserved by this lane",
		activate: func() ([]string, error) {
			if !fileExists(caPath) {
				return nil, fmt.Errorf("the transport relay is listening on %s but its CA certificate is not at %s; "+
					"refusing to set NODE_EXTRA_CA_CERTS to a file that does not exist, because every intercepted "+
					"handshake would then fail and look like the provider being down. Check %s",
					addr, caPath, spec.LogPath(homeDir))
			}
			keys := activation.TransportKeys(addr, caPath, activation.CurrentEnv(settings))
			res, err := activation.Activate(homeDir, settings, activation.LaneTransport, keys)
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(a.stdout, "  %-14s %d keys -> %s (user scope: %s)\n",
				"transport env", len(keys), "http://"+addr, settings)
			fmt.Fprintf(a.stdout, "  %-14s %s\n", "transport CA", caPath)
			fmt.Fprintf(a.stdout, "                 this INTERCEPTS the provider's TLS on this machine. Every other host is\n")
			fmt.Fprintf(a.stdout, "                 tunnelled uninspected.\n")
			return res.Replaced, nil
		},
	})
}

func (a *app) removeTransport(homeDir string, force bool) error {
	spec := laneservice.Transport(transport.DefaultAddr, "", false)
	settings := claudeSettingsPath(homeDir)
	return a.removeLane(laneRemoval{
		label:         "transport",
		laneIdentity:  identityOf(spec, homeDir),
		uninstallUnit: func() error { return uninstallLaneUnitFn(spec, runtime.GOOS, homeDir) },
		deactivate: func() error {
			return a.reportDeactivation("transport", homeDir, settings, activation.LaneTransport, force)
		},
	})
}

func (a *app) reportDeactivation(label, homeDir, settingsPath string, lane activation.Lane, force bool) error {
	res, err := activation.Deactivate(homeDir, settingsPath, lane, force)
	if err != nil {
		return err
	}
	if len(res.Removed) > 0 {
		fmt.Fprintf(a.stdout, "  removed        %d %s env key(s) from %s\n", len(res.Removed), label, settingsPath)
	}
	for key, value := range res.Restored {
		fmt.Fprintf(a.stdout, "  restored       %s = %s (the value that was there before OpenBox)\n", key, value)
	}
	return nil
}

// claudeSettingsPath resolved through gatewayservice so the three lanes and
// doctor cannot disagree about which file they are all editing.
func claudeSettingsPath(homeDir string) string { return gatewaySettingsPath(homeDir) }
