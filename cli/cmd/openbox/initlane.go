package main

import (
	"fmt"
	"runtime"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/activation"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/laneservice"
	"github.com/openbox-ai/openbox-shift-left/telemetry"
	"github.com/openbox-ai/openbox-shift-left/transport"
)

// initlane.go is the install and removal choreography every supervised lane
// runs, and the two commands OD2 asked for: one in, one out.
//
// ── THE ORDER IS A SAFETY PROPERTY, NOT A STYLE ───────────────────────────────
//
//	unit -> start -> PROVE it listens -> only then write the env keys.
//
// Writing the env first points the tool at a port nothing answers. For the
// in-path lanes a dead localhost fails closed, so EVERY model call on the machine
// fails and `init` reports success. So the env write is last and conditional, and
// if the daemon cannot be proven up the keys are not written at all — the machine
// is left exactly as it was, unobserved for model calls and WORKING.
//
// The reverse order on REMOVAL, for the same reason: env first, then the daemon,
// so there is never a window where the tool points at something gone.
//
// Any failure after the unit is written must also REMOVE the unit. KeepAlive and
// Restart=always mean a unit left behind restart-loops a daemon the developer was
// never told about, and `init` still exits 0 because the caller downgrades this
// to a warning.

// setupLane installs, starts and verifies one supervised lane, then activates its
// env keys. Any failure leaves the machine unconfigured rather than
// half-configured.
type laneInstall struct {
	// label is the lane's name in every line this prints.
	label string
	// addr is the loopback address the daemon must be listening on before any env
	// key is written.
	addr string
	// homeDir is the developer's home.
	homeDir string
	// laneIdentity is how the supervisor addresses THIS lane. Passed rather than
	// derived because `systemctl --user enable` names a unit and `launchctl
	// bootout` names a label: one hardcoded identity here would start or stop the
	// wrong daemon and report success.
	laneIdentity
	// installUnit and uninstallUnit are closures rather than a shared seam
	// because the gateway's own seam predates this function and its tests bind it
	// directly. One choreography, per-lane mechanics.
	installUnit   func() error
	uninstallUnit func() error
	// activate writes the lane's env keys and returns what it displaced. Called
	// LAST, only once the daemon is proven up. It prints its own summary line;
	// the displaced values are printed here so every lane reports them the same.
	activate func() (replaced []string, err error)
	// envNotSet completes the sentence "… was NOT written, so …" in every failure
	// message. A developer reading an install failure has to be told what still
	// works, or they start undoing things that were fine.
	envNotSet string
}

// laneIdentity is everything the OS supervisor needs to address one lane.
type laneIdentity struct {
	unitPath     string
	launchdLabel string
	systemdUnit  string
}

// identityOf reads a lane's supervisor identity off its spec, so the install,
// the load, the unload and the removal cannot disagree about which daemon they
// are talking about.
func identityOf(spec laneservice.Spec, homeDir string) laneIdentity {
	return laneIdentity{
		unitPath:     spec.UnitPath(runtime.GOOS, homeDir),
		launchdLabel: spec.Label,
		systemdUnit:  spec.SystemdUnitName(),
	}
}

func (a *app) setupLane(in laneInstall) error {
	// Is the port already taken by something else? Checked BEFORE starting,
	// because the readiness probe below cannot tell our daemon from a stranger: it
	// is a bare TCP connect. Without this, a foreign process holding the port
	// means our daemon fails to bind, the probe connects to the stranger anyway,
	// and init points the developer's traffic at an unknown local service while
	// reporting success.
	//
	// OUR OWN daemon holding the port is a RE-INSTALL, not a conflict, and
	// ownership is decided by OUR UNIT rather than by probing the socket: if the
	// unit we wrote names this same address, whatever answers there is ours to
	// replace. A port held by anything else is still refused — over-refuse, never
	// over-terminate.
	if occupied, who := portOccupied(in.addr); occupied {
		if !unitDescribesAddr(in.unitPath, in.addr) {
			return fmt.Errorf("%s is already in use%s — refusing to continue, because the readiness check "+
				"cannot tell our %s from whatever is listening. Stop it, or choose another address",
				in.addr, who, in.label)
		}
		fmt.Fprintf(a.stdout, "  replacing      the %s already installed at %s (%s)\n", in.label, in.addr, in.unitPath)
		a.unloadUnit(in.laneIdentity)
		if !waitForPortFreeFn(in.addr, gatewayStopTimeout) {
			return fmt.Errorf("the %s already running on %s did not stop within %s — nothing was changed. "+
				"Stop it by hand and re-run init", in.label, in.addr, gatewayStopTimeout)
		}
	}

	if err := in.installUnit(); err != nil {
		// Includes the Windows case, which is an error rather than a silent skip:
		// a developer who believes a service was installed and finds none later is
		// worse off than one told plainly.
		return err
	}
	fmt.Fprintf(a.stdout, "  %-14s %s\n", in.label+" unit", in.unitPath)

	if err := a.loadUnit(in.laneIdentity); err != nil {
		a.rollbackLaneUnit(in)
		return fmt.Errorf("wrote %s but could not start it (%w) — %s. Start it by hand with "+
			"`openbox %s`, then re-run init", in.unitPath, err, in.envNotSet, in.label)
	}

	if !waitForListenerFn(in.addr, gatewayReadyTimeout) {
		a.rollbackLaneUnit(in)
		return fmt.Errorf("the %s did not start listening on %s within %s — %s. Check the service logs, "+
			"or run `openbox %s` in the foreground to see why", in.label, in.addr, gatewayReadyTimeout, in.envNotSet, in.label)
	}
	fmt.Fprintf(a.stdout, "  %-14s listening on %s\n", in.label, in.addr)

	// Only now. Everything above had to succeed for this to be safe.
	replaced, err := in.activate()
	if err != nil {
		a.rollbackLaneUnit(in)
		return err
	}
	for _, r := range replaced {
		// Never silent: the developer or their org had pointed this elsewhere.
		fmt.Fprintf(a.stdout, "  replaced       %s\n", r)
	}
	return nil
}

// rollbackLaneUnit undoes a unit that was written and then could not be made to
// work. Best-effort and silent about its own failures: this runs on a path that
// is already reporting an error, and a second error about cleaning up the first
// would bury it.
func (a *app) rollbackLaneUnit(in laneInstall) {
	if in.unitPath == "" {
		return
	}
	a.unloadUnit(in.laneIdentity)
	if err := in.uninstallUnit(); err == nil {
		fmt.Fprintf(a.stdout, "  rolled back    removed %s\n", in.unitPath)
	}
}

// laneRemoval is the uninstall half, in the opposite order.
type laneRemoval struct {
	label string
	laneIdentity
	// deactivate restores the lane's env keys. It runs FIRST and prints its own
	// account of what it removed and what it restored — a restore is not a
	// removal, and a developer told "removed" about a value that was actually put
	// back will go looking for the wrong thing.
	deactivate    func() error
	uninstallUnit func() error
}

func (a *app) removeLane(in laneRemoval) error {
	if in.deactivate != nil {
		if err := in.deactivate(); err != nil {
			return err
		}
	}
	// Unload BEFORE deleting the file. launchctl's fallback spelling
	// (`launchctl unload <path>`) has to READ the plist to identify the job, so
	// doing this after the delete leaves the fallback structurally unable to help:
	// if the primary `bootout` failed, a KeepAlive job could stay running and
	// restarting with no unit on disk, silently, while init reported success.
	a.unloadUnit(in.laneIdentity)
	if err := in.uninstallUnit(); err != nil {
		return err
	}
	if in.unitPath != "" {
		fmt.Fprintf(a.stdout, "  removed        %s\n", in.unitPath)
	}
	return nil
}

// installLaneUnitFn / uninstallLaneUnitFn are the unit-file seam for the two
// lanes added here.
//
// Production goes through kardianos/service (D-OSS-3). That library derives the
// darwin plist path from user.Current() and IGNORES $HOME, with no override — so
// calling it from a test would write a live launchd unit into the home directory
// of whoever ran `go test`, including CI. The tests that drive these installs are
// the ones that prove the safety property this repo cares most about — that the
// env is never written while nothing listens, and that a failed start leaves no
// unit behind — so they route through the path-explicit writer instead. Both
// produce identical bytes: the bodies come from the same renderers, and
// laneservice.TestSuppliedTemplatesSurviveRendering pins that the library's
// template render is an identity transform over them.
var installLaneUnitFn = func(spec laneservice.Spec, goos, homeDir, binPath string) error {
	return spec.Reinstall(goos, homeDir, binPath)
}

var uninstallLaneUnitFn = func(spec laneservice.Spec, goos, homeDir string) error {
	return spec.Uninstall(goos, homeDir)
}

// ── telemetry ────────────────────────────────────────────────────────────────

// setupTelemetry installs the local OTLP receiver and points the tool's own
// telemetry at it.
//
// This lane is ADDITIVE: it observes the tool reporting on itself, so a failure
// here degrades to "no model-call evidence from this lane", never to a broken
// tool. That is also its weakness, and why OD4 makes silence a finding rather
// than an absence.
func (a *app) setupTelemetry(homeDir, addr string, verbose bool) error {
	spec := laneservice.Telemetry(addr, verbose)
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
			return res.Replaced, nil
		},
	})
}

func (a *app) removeTelemetry(homeDir string, force bool) error {
	spec := laneservice.Telemetry(telemetry.DefaultAddr, false)
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

// ── transport ────────────────────────────────────────────────────────────────

// setupTransport installs the in-path relay and points the tool's proxy settings
// and CA trust at it.
//
// The CA is checked to EXIST before any env key names it. The daemon generates it
// on first start, before it listens, so proving the listener also proves the CA —
// but "also proves" is an inference, and a NODE_EXTRA_CA_CERTS pointing at a file
// that is not there fails every intercepted handshake, which the developer
// experiences as the provider being down rather than as a config error.
func (a *app) setupTransport(homeDir, addr string, verbose bool) error {
	spec := laneservice.Transport(addr, verbose)
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
				return nil, fmt.Errorf("the transport relay is listening on %s but its CA certificate is not at %s — "+
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
	spec := laneservice.Transport(transport.DefaultAddr, false)
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

// ── shared reporting ─────────────────────────────────────────────────────────

// reportDeactivation restores a lane's env keys and says exactly what happened to
// each one.
func (a *app) reportDeactivation(label, homeDir, settingsPath string, lane activation.Lane, force bool) error {
	res, err := activation.Deactivate(homeDir, settingsPath, lane, force)
	if err != nil {
		return err
	}
	if len(res.Removed) > 0 {
		fmt.Fprintf(a.stdout, "  removed        %d %s env key(s) from %s\n", len(res.Removed), label, settingsPath)
	}
	for key, value := range res.Restored {
		// A restore has to be SAID. It is the opposite of a removal — the machine
		// is back to what the org configured, not unconfigured.
		fmt.Fprintf(a.stdout, "  restored       %s = %s (the value that was there before OpenBox)\n", key, value)
	}
	return nil
}

// claudeSettingsPath is the one settings file all three lanes write.
//
// USER scope, not project scope, and that is the ADR-0016 amendment rather than
// a preference: these variables are read from managed settings and
// ~/.claude/settings.json, and background agents need settings rather than shell
// exports. A project-scoped write would report success while governing nothing.
//
// Resolved through gatewayservice so the three lanes and doctor cannot disagree
// about which file they are all editing.
func claudeSettingsPath(homeDir string) string { return gatewaySettingsPath(homeDir) }
