package main

import (
	"fmt"
	"runtime"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

type laneInstall struct {
	label   string
	addr    string
	homeDir string
	laneIdentity
	installUnit   func() error
	uninstallUnit func() error
	activate      func() (activated, error)
	envNotSet     string
	// unitExisted is set by setupLane before it writes the unit.
	unitExisted bool
}

// activated is what routing a lane changed. It is returned rather than
// printed because a success prints one summary for every lane at the end; a
// replaced value is the exception, being the one thing here that belonged to
// somebody else.
type activated struct {
	keys     int
	replaced []string
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

func (a *app) setupLane(in laneInstall) (int, error) {
	// Checked before starting, because the readiness probe below cannot tell our
	// daemon from a stranger: it is a bare TCP connect.
	if occupied, who := portOccupied(in.addr); occupied {
		if !unitDescribesAddr(in.unitPath, in.addr) {
			return 0, fmt.Errorf("%s is already in use%s; refusing to continue, because the readiness check "+
				"cannot tell our %s from whatever is listening. Stop it, or choose another address",
				in.addr, who, in.label)
		}
		a.row("replacing", "the %s already installed at %s (%s)", in.label, in.addr, in.unitPath)
		a.unloadUnit(in.laneIdentity)
		if !waitForPortFreeFn(in.addr, gatewayStopTimeout) {
			return 0, fmt.Errorf("the %s already running on %s did not stop within %s; nothing was changed. "+
				"Stop it by hand and re-run init", in.label, in.addr, gatewayStopTimeout)
		}
	}

	// A unit that was already there is not ours to remove on a later failure: it
	// is the shared receiver, and a working lane of another tool may depend on it.
	in.unitExisted = in.unitPath != "" && fileExists(in.unitPath)
	if err := in.installUnit(); err != nil {
		traceUnit(in.label, "write", err, map[string]any{"unit": in.unitPath})
		return 0, err
	}
	traceUnit(in.label, "write", nil, map[string]any{"unit": in.unitPath})

	if err := a.loadUnit(in.laneIdentity); err != nil {
		traceUnit(in.label, "start", err, map[string]any{"unit": in.unitPath})
		a.rollbackLaneUnit(in)
		return 0, fmt.Errorf("wrote %s but could not start it (%w); %s. Start it by hand with "+
			"`openbox %s`, then re-run init", in.unitPath, err, in.envNotSet, in.label)
	}
	traceUnit(in.label, "start", nil, map[string]any{"unit": in.unitPath})

	// The env keys go in only once something is listening, and that order is a
	// safety property rather than a nicety: see TestLaneEnvIsWrittenAfterReadiness.
	if !waitForListenerFn(in.addr, gatewayReadyTimeout) {
		listenErr := fmt.Errorf("did not start listening on %s within %s", in.addr, gatewayReadyTimeout)
		traceUnit(in.label, "listen-proof", listenErr, map[string]any{"addr": in.addr})
		a.rollbackLaneUnit(in)
		return 0, fmt.Errorf("the %s did not start listening on %s within %s; %s. Check the service logs, "+
			"or run `openbox %s` in the foreground to see why", in.label, in.addr, gatewayReadyTimeout, in.envNotSet, in.label)
	}
	traceUnit(in.label, "listen-proof", nil, map[string]any{"addr": in.addr})

	act, err := in.activate()
	traceActivation(in.label, err, nil, map[string]any{"replaced": act.replaced})
	if err != nil {
		a.rollbackLaneUnit(in)
		return 0, err
	}
	for _, r := range act.replaced {
		a.row("replaced", "%s", r)
	}
	return act.keys, nil
}

func (a *app) rollbackLaneUnit(in laneInstall) {
	if in.unitPath == "" {
		return
	}
	if in.unitExisted {
		// Removing it would silently end a telemetry lane that still works for
		// the tools whose settings point at its port. It was just rewritten with
		// a superset of its old paths and restarted, so leaving it is safe.
		a.row("kept", "%s: it was already installed, so a failure of this step does not remove it", in.unitPath)
		return
	}
	a.unloadUnit(in.laneIdentity)
	err := in.uninstallUnit()
	traceUnit(in.label, "remove", err, map[string]any{"unit": in.unitPath, "rollback": true})
	if err == nil {
		a.row("rolled back", "removed %s", in.unitPath)
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
			traceActivation(in.label, err, nil, map[string]any{"action": "deactivate"})
			return err
		}
		traceActivation(in.label, nil, nil, map[string]any{"action": "deactivate"})
	}
	a.unloadUnit(in.laneIdentity)
	if err := in.uninstallUnit(); err != nil {
		traceUnit(in.label, "remove", err, map[string]any{"unit": in.unitPath})
		return err
	}
	traceUnit(in.label, "remove", nil, map[string]any{"unit": in.unitPath})
	if in.unitPath != "" {
		a.row("removed", "%s", in.unitPath)
	}
	return nil
}

// advisoryFileEnvKey is the env name hookflow.DefaultAdvisoryPath() itself
// checks first (hookflow/advisory.go); named here, not imported, because
// hookflow does not export it as a constant -- this is the same value, kept
// in sync by grep (there is exactly one other literal of it in the repo).
const advisoryFileEnvKey = "OPENBOX_ADVISORY_FILE"

// laneUnitEnv is the coordinate environment the unit has to carry, captured
// from the installing process. A supervisor starts a daemon with no
// environment of its own, so devconfig.Home() inside it resolves the real
// ~/.openbox while this process validated the path its own $OPENBOX_HOME
// named — and the transport CA check then refuses a certificate that is not
// where it looked.
//
// Seven keys now, and only path coordinates: a unit file is world-readable,
// so the API key and the signing seed must never reach one. The first two
// (OPENBOX_HOME, OPENBOX_SPOOL_DIR) are overrides this process's own
// environment may or may not carry, so they are copied only when set. The
// remaining five are different in kind: each is RESOLVED rather than
// copied, because a daemon has no $HOME at all -- their shared
// os.UserConfigDir() fallback (through devconfig.ConfigDir()) would resolve
// differently, or not at all, inside it.
// Without OPENBOX_SESSION_DIR the gateway/proxy lanes would read (or bump) a
// run record this process never wrote and disagree with the hooks about
// which run a call belongs to; without OPENBOX_HALT_DIR a lane daemon's own
// HALT-latch write, and the transport lane's cross-lane read of it, would
// each resolve a DIFFERENT, wrong directory instead of the one hooks already
// write to; without OPENBOX_ENFORCEMENT_FILE the transport lane's record of a
// latched refusal would land somewhere no one reads, or nowhere; without
// OPENBOX_SPOOL_ROOT a lane record's own LaneQueue would spool into a
// DIFFERENT directory than the one that tool's hook events already queue
// through, breaking the append-order interleave delivery ordering relies
// on; without
// OPENBOX_ADVISORY_FILE the daemon's own Advisory sink (wired by
// hookflow.NewEngine into every LaneQueue's own Engine) would resolve a
// bogus relative path (".config/openbox/advisories.jsonl") instead of
// erroring loudly, since a daemon's $HOME is not this process's own.
//
// Eight keys now: trace.EnvDir joins the resolved five, for the same reason
// -- a daemon has no $HOME to derive `devconfig.ConfigDir()/trace` from, and
// without it a lane daemon would trace into wherever ITS OWN empty-HOME
// resolution landed instead of this installer's, or discard every record
// silently if that resolution failed outright. Read from trace.Dir(), which
// main.go's resolveTraceDir already set for THIS process (env override or
// the config-dir default) -- resolved once, not re-derived here.
func (a *app) laneUnitEnv() map[string]string {
	env := make(map[string]string, 8)
	for _, key := range []string{devconfig.EnvHome, devconfig.EnvSpoolDir} {
		if v := a.getenv(key); v != "" {
			env[key] = v
		}
	}
	env[obgit.EnvSessionDir] = obgit.DefaultSessionDir()
	env[devconfig.EnvHaltDir] = hookflow.DefaultHaltDir()
	env[devconfig.EnvEnforcementFile] = hookflow.DefaultEnforcementPath()
	env[devconfig.EnvSpoolRoot] = devconfig.ConfigDir()
	env[advisoryFileEnvKey] = hookflow.DefaultAdvisoryPath()
	env[trace.EnvDir] = trace.Dir()
	return env
}

var installLaneUnitFn = func(spec laneservice.Spec, goos, homeDir, binPath string) error {
	return spec.Reinstall(goos, homeDir, binPath)
}

var uninstallLaneUnitFn = func(spec laneservice.Spec, goos, homeDir string) error {
	return spec.Uninstall(goos, homeDir)
}

// telemetrySpec is the ONE shared telemetry unit, for whichever tool's install
// is building it. The unit carries every tool's settings path, because one
// receiver serves them all and a daemon has no $HOME to re-derive any of them:
// an install for one tool must therefore keep the paths of the tools already
// installed, or the later install silently strips the earlier tool's election.
//
// A tool's path rides the unit when that tool is the one installing, or when
// OpenBox already owns its telemetry surface (Codex's [otel] block, Muse's
// telemetry object): CodexConfigTOMLPath and MuseSettingsPath always resolve to
// a path, so passing them unconditionally would bake each tool's whole wiring
// into the unit of a machine that never touched it. Either install order gives
// the same unit.
func (a *app) telemetrySpec(homeDir, addr string, verbose bool, installing provider.Name) laneservice.Spec {
	spec := laneservice.Telemetry(addr, claudeSettingsPath(homeDir), verbose).
		WithEnv(a.laneUnitEnv())
	if codexConfigPath := providers.CodexConfigTOMLPath(); installing == provider.Codex || providers.HasOwnedCodexOtel(codexConfigPath) {
		// The record path rides with the config path: Codex's election reads both.
		spec = spec.WithCodexSettings(codexConfigPath).WithPACRecord(activation.RecordPath(homeDir))
	}
	if musePath := providers.MuseSettingsPath(); installing == provider.Muse || providers.HasOwnedMuseTelemetry(musePath, homeDir) {
		spec = spec.WithMuseSettings(musePath)
	}
	return spec
}

// setupTelemetry installs the local OTLP receiver and points the tool's own
// telemetry at it.
func (a *app) setupTelemetry(homeDir, addr string, verbose bool) (int, error) {
	spec := a.telemetrySpec(homeDir, addr, verbose, provider.ClaudeCode)
	binPath, err := a.selfPath()
	if err != nil {
		return 0, err
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
		activate: func() (activated, error) {
			keys := activation.TelemetryKeys(addr)
			res, err := activation.Activate(homeDir, settings, activation.LaneTelemetry, keys)
			if err != nil {
				return activated{}, err
			}
			return activated{keys: len(keys), replaced: res.Replaced}, nil
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

// setupCodexTelemetry installs the SAME local OTLP receiver setupTelemetry
// does (one receiver, two tools) and points Codex's own OTel exporter at it
// by merging an owned [otel] block into config.toml -- never settings.json,
// which is Claude Code's surface. Install ordering matches setupTelemetry
// exactly, because it is the same setupLane: unit -> start -> prove
// listening -> write the pointer, and any failure after WriteUnit removes
// the unit.
func (a *app) setupCodexTelemetry(homeDir, addr string, verbose bool) (int, error) {
	spec := a.telemetrySpec(homeDir, addr, verbose, provider.Codex)
	binPath, err := a.selfPath()
	if err != nil {
		return 0, err
	}
	configPath := providers.CodexConfigTOMLPath()
	endpoint := "http://" + addr + "/v1/logs"

	return a.setupLane(laneInstall{
		label:         "telemetry",
		addr:          addr,
		homeDir:       homeDir,
		laneIdentity:  identityOf(spec, homeDir),
		installUnit:   func() error { return installLaneUnitFn(spec, runtime.GOOS, homeDir, binPath) },
		uninstallUnit: func() error { return uninstallLaneUnitFn(spec, runtime.GOOS, homeDir) },
		envNotSet: "the Codex telemetry pointer was NOT written, so Codex exports nothing and this " +
			"lane records nothing for it; Claude Code's own lane is unaffected",
		activate: func() (activated, error) {
			if err := providers.WriteCodexOtel(configPath, endpoint); err != nil {
				return activated{}, err
			}
			return activated{keys: 1}, nil
		},
	})
}

// setupMuseTelemetry installs the SAME local OTLP receiver and points Muse's
// own telemetry export at it by merging an owned `telemetry` object into Muse's
// settings.json -- never Claude Code's, never an env var (Muse ignores
// OTEL_EXPORTER_OTLP_ENDPOINT). Install ordering is setupLane's: unit -> start
// -> prove listening -> write the pointer, and any failure after WriteUnit
// removes the unit. The pointer write records Muse's previous value first.
func (a *app) setupMuseTelemetry(homeDir, addr string, verbose bool) (int, error) {
	spec := a.telemetrySpec(homeDir, addr, verbose, provider.Muse)
	binPath, err := a.selfPath()
	if err != nil {
		return 0, err
	}
	settingsPath := providers.MuseSettingsPath()
	endpoint := "http://" + addr
	// Refusals that depend only on Muse's files are decided BEFORE the shared
	// unit is rewritten, so they never disturb a telemetry lane that works.
	if err := providers.CheckMuseTelemetry(settingsPath, homeDir, endpoint); err != nil {
		return 0, fmt.Errorf("%w; nothing was changed, and the telemetry unit was not touched", err)
	}

	return a.setupLane(laneInstall{
		label:         "telemetry",
		addr:          addr,
		homeDir:       homeDir,
		laneIdentity:  identityOf(spec, homeDir),
		installUnit:   func() error { return installLaneUnitFn(spec, runtime.GOOS, homeDir, binPath) },
		uninstallUnit: func() error { return uninstallLaneUnitFn(spec, runtime.GOOS, homeDir) },
		envNotSet: "Muse's telemetry setting was NOT written, so Muse exports nothing to this machine and this " +
			"lane records nothing for it; Muse's hooks and the other tools' lanes are unaffected",
		activate: func() (activated, error) {
			replaced, err := providers.WriteMuseTelemetry(settingsPath, homeDir, endpoint)
			if err != nil {
				return activated{}, err
			}
			res := activated{keys: 1}
			if replaced != "" {
				res.replaced = []string{fmt.Sprintf("Muse's telemetry setting %s in %s; `openbox uninstall` puts it back",
					replaced, settingsPath)}
			}
			return res, nil
		},
	})
}

// setupTransport installs the relay for Claude Code, the provider every
// caller of the lower-level setup means.
func (a *app) setupTransport(homeDir, addr string, verbose bool) (int, error) {
	return a.setupTransportFor(provider.ClaudeCode, homeDir, addr, verbose)
}

// setupTransportFor installs the relay's unit on behalf of one provider's
// `init`. The unit always carries the derived provider set, so installing one
// provider never drops another's routing.
//
// Claude Code's install also routes its env block at the relay and activates
// the system PAC inside the lane's own activate step. Codex has no env block
// to route: its routing IS the system PAC, which is activated last, after
// both daemons are on the new binary (setupLanes), so this only installs and
// proves the unit, and writes nothing Codex reads.
func (a *app) setupTransportFor(installing provider.Name, homeDir, addr string, verbose bool) (int, error) {
	set := derivedTransportProviders(homeDir, installing)
	spec := laneservice.Transport(addr, claudeSettingsPath(homeDir), verbose).
		WithProviders(set).
		WithEnv(a.laneUnitEnv())
	if hasProvider(set, provider.Codex) {
		// Both paths come from here, never a re-derivation in the daemon,
		// which has no $HOME.
		spec = spec.WithCodexSettings(providers.CodexConfigTOMLPath()).WithPACRecord(activation.RecordPath(homeDir))
	}
	binPath, err := a.selfPath()
	if err != nil {
		return 0, err
	}
	openboxHome, err := devconfig.Home()
	if err != nil {
		return 0, err
	}
	caPath, _ := transport.CAPaths(openboxHome)
	settings := claudeSettingsPath(homeDir)

	// Reset per call: the system PAC step is the only writer, and only when it
	// is actually reached (readiness proven) -- see runSystemPACActivation's
	// own doc for why a listen failure or a rollback must never touch this. A
	// stale value from an earlier call in the same process must never be read
	// as this one's.
	a.lastSystemPACOutcome = activation.Outcome{}

	caMissing := func() error {
		return fmt.Errorf("the transport relay is listening on %s but its CA certificate is not at %s; "+
			"refusing to set NODE_EXTRA_CA_CERTS to a file that does not exist, because every intercepted "+
			"handshake would then fail and look like the provider being down. Check %s",
			addr, caPath, spec.LogPath(homeDir))
	}
	return a.setupLane(laneInstall{
		label:         "transport",
		addr:          addr,
		homeDir:       homeDir,
		laneIdentity:  identityOf(spec, homeDir),
		installUnit:   func() error { return installLaneUnitFn(spec, runtime.GOOS, homeDir, binPath) },
		uninstallUnit: func() error { return uninstallLaneUnitFn(spec, runtime.GOOS, homeDir) },
		envNotSet: "the proxy env keys were NOT written, so model calls still reach the provider " +
			"directly and are unobserved by this lane",
		activate: func() (activated, error) {
			if !fileExists(caPath) {
				return activated{}, caMissing()
			}
			if installing == provider.Codex {
				return activated{}, nil
			}
			keys := activation.TransportKeys(addr, caPath, activation.CurrentEnv(settings))
			res, err := activation.Activate(homeDir, settings, activation.LaneTransport, keys)
			if err != nil {
				return activated{}, err
			}
			// The system PAC step, strictly after the env keys above: a failure
			// here must leave a working, env-routed transport lane rather than
			// rolling back what already succeeded. Its outcome is read back by
			// setupLanes via a.lastSystemPACOutcome once this call returns; see
			// setupTransportFor's own reset of that field.
			if a.withholdSystemPAC {
				fmt.Fprintf(a.stderr, "warning: the system PAC was not activated: the telemetry lane did not "+
					"come up on this binary, and an older telemetry daemon would keep recording the Codex "+
					"calls the relay is about to record too. Fix the telemetry lane and re-run `openbox init`\n")
				return activated{keys: len(keys), replaced: res.Replaced}, nil
			}
			a.lastSystemPACOutcome = a.runSystemPACActivation(homeDir, addr, caPath, set)
			return activated{keys: len(keys), replaced: res.Replaced}, nil
		},
	})
}

// activateCodexSystemPAC is the last step of `init --provider codex`: point
// the system at the relay and commit the record that lists Codex. Nothing is
// decided by this alone: Codex's election also waits for the relay to see a
// Codex call.
func (a *app) activateCodexSystemPAC(homeDir, addr string) activation.Outcome {
	openboxHome, err := devconfig.Home()
	if err != nil {
		return activation.Outcome{Class: activation.Failed, Reason: err.Error()}
	}
	caPath, _ := transport.CAPaths(openboxHome)
	return a.runSystemPACActivation(homeDir, addr, caPath, derivedTransportProviders(homeDir, provider.Codex))
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
		a.row("removed", "%d %s env key(s) from %s", len(res.Removed), label, settingsPath)
	}
	for key, value := range res.Restored {
		a.row("restored", "%s = %s (the value that was there before OpenBox)", key, value)
	}
	return nil
}

// claudeSettingsPath resolved through gatewayservice so the three lanes and
// doctor cannot disagree about which file they are all editing.
func claudeSettingsPath(homeDir string) string { return gatewayservice.SettingsPath(homeDir) }
