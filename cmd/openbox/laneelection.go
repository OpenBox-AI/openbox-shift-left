package main

import (
	"io"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayservice"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// electionChangeSentinel never equals a real election result (electedName
// renders "" for "nobody", never this), so the very first resolution always
// counts as a change and gets traced -- a reader of the trace otherwise could
// not tell "this lane has always been elected" from "the election just
// resolved for the first time".
const electionChangeSentinel = "\x00unresolved\x00"

// laneSettingsPath resolves where a lane daemon reads the tool's settings from.
func (a *app) laneSettingsPath(fromUnit string) string {
	if fromUnit != "" {
		return fromUnit
	}
	return gatewayservice.SettingsPath(a.homeDir())
}

// electedFn is the per-record election gate. A function, not a bool: install
// starts the daemon before writing the env var.
func electedFn(settingsPath string, lane activation.Lane, override *bool) func() bool {
	return electedBy(func() activation.Election { return activation.ResolveElection(settingsPath) }, lane, override)
}

// electedBy is the per-record election gate over one election source: true
// when the operator forced it (--elected) or resolve names lane. resolve runs
// per call, never cached.
func electedBy(resolve func() activation.Election, lane activation.Lane, override *bool) func() bool {
	return func() bool {
		if override != nil && *override {
			return true
		}
		return resolve().Elected == lane
	}
}

// electedNameBy reports which lane resolve names, "" for none -- lane itself
// when the operator forced it -- and traces each change under traceLane.
func electedNameBy(resolve func() activation.Election, lane activation.Lane, traceLane string, override *bool) func() string {
	var mu sync.Mutex
	last := electionChangeSentinel
	return func() string {
		var name string
		if override != nil && *override {
			name = string(lane) // forced by the operator; this lane is the producer
		} else {
			name = string(resolve().Elected)
		}
		traceElectionChange(&mu, &last, traceLane, name)
		return name
	}
}

// electionProblemBy reports why resolve could not decide, "" when the operator
// decided it and there is nothing to read.
func electionProblemBy(resolve func() activation.Election, override *bool) func() string {
	return func() string {
		if override != nil && *override {
			return ""
		}
		return resolve().SettingsProblem
	}
}

// electedNameFn reports WHICH lane the election named, "" for none.
//
// The emitter needs this to tell "another lane won" -- healthy, and quiet -- from
// "the settings route no lane at all", where nothing emits and a relay holding a
// call is a routing gap worth saying out loud.
func electedNameFn(settingsPath string, lane activation.Lane, override *bool) func() string {
	return electedNameBy(func() activation.Election { return activation.ResolveElection(settingsPath) }, lane, string(lane), override)
}

// traceElectionChange emits trace.StageElection the first time it is called
// for a given closure (last starts at electionChangeSentinel) and every time
// the resolved elected-lane name actually changes afterward -- never on
// every re-resolution, which would flood the trace with one record per
// relayed call for a settings file that never moved.
func traceElectionChange(mu *sync.Mutex, last *string, lane, elected string) {
	mu.Lock()
	changed := *last != elected
	*last = elected
	mu.Unlock()
	if !changed {
		return
	}
	trace.Emit(trace.Record{
		Stage:   trace.StageElection,
		Lane:    lane,
		Outcome: elected,
		Detail:  map[string]any{"elected": elected},
	})
}

// electionProblemFn reports why the election could not be decided, or "".
func electionProblemFn(settingsPath string, override *bool) func() string {
	return electionProblemBy(func() activation.Election { return activation.ResolveElection(settingsPath) }, override)
}

// museElectedFn is electedFn's Muse counterpart: it reads Muse's settings.json
// telemetry block through activation.ResolveMuseElection. Muse has one lane, so
// the answer is whether Muse's own export is pointed at THIS receiver
// (receiverAddr); the settings path comes from the unit's argv, never from
// $HOME, which a daemon does not have.
func museElectedFn(settingsPath, receiverAddr string, override *bool) func() bool {
	return electedBy(func() activation.Election { return activation.ResolveMuseElection(settingsPath, receiverAddr) }, activation.LaneTelemetry, override)
}

// reportMuseElection logs the telemetry daemon's startup view of Muse's
// election, the way reportCodexElection does for Codex.
func reportMuseElection(logger *log.Logger, settingsPath, receiverAddr string, override bool) {
	e := activation.ResolveMuseElection(settingsPath, receiverAddr)
	switch {
	case override:
		logger.Printf("openbox telemetry: emitting Muse model-call turns because --elected was passed, "+
			"overriding the election (which currently names %q)", orNone(string(e.Elected)))
	case e.SettingsProblem != "":
		logger.Printf("openbox telemetry: CANNOT DECIDE whether to emit Muse model-call turns: %s. "+
			"Reinstall with `openbox init --provider muse` so the unit carries --muse-settings, or pass --elected.",
			e.SettingsProblem)
	case e.Elected == activation.LaneTelemetry:
		logger.Printf("openbox telemetry: elected producer of Muse model-call turns; %s", e.Reason)
	default:
		logger.Printf("openbox telemetry: NOT the elected producer of Muse model-call turns (%s); "+
			"emitting none. Re-checked per call, so this changes on its own once Muse's settings "+
			"are written -- install starts this daemon before they are.", e.Reason)
	}
}

// codexRelaySpoolSubdir and codexObservedMarker name where the relay's
// evidence that it has seen Codex lives: inside Codex's own spool directory,
// the same place (and the same resolution, laneSpoolDir) the run-started
// markers live, so both daemons find one file without any path in their
// unit and uninstall's spool sweep removes it with everything else.
const (
	codexRelaySpoolSubdir = "codex-spool"
	codexObservedMarker   = "relay-observed/codex"
)

// codexElectionPaths are the three files Codex's election reads. Two come
// from the unit's argv (a daemon has no $HOME to derive either from); the
// third is resolved by the daemon the way it resolves every spool path.
type codexElectionPaths struct {
	config, pacRecord, marker string
}

func newCodexElectionPaths(config, pacRecord string, logger *log.Logger) codexElectionPaths {
	return codexElectionPaths{
		config:    config,
		pacRecord: pacRecord,
		marker:    filepath.Join(laneSpoolDir(codexRelaySpoolSubdir, logger), filepath.FromSlash(codexObservedMarker)),
	}
}

// resolve is the one call every consumer makes, so the two daemons and
// doctor cannot read the three files differently.
func (p codexElectionPaths) resolve() activation.Election {
	return activation.ResolveCodexElection(p.config, p.pacRecord, p.marker)
}

// codexElectedFn is electedFn's Codex counterpart: it reads config.toml and the
// system-PAC record through activation.ResolveCodexElection, never
// settings.json through ResolveElection -- the native surfaces must never be
// read through each other's parser, or a TOML file handed to the JSON reader
// reports "cannot decide" forever. lane is the lane the calling daemon speaks
// for: telemetry asks for LaneTelemetry and the relay for LaneTransport, and
// because both resolve the same three files exactly one of them is elected.
func codexElectedFn(paths codexElectionPaths, lane activation.Lane, override *bool) func() bool {
	return electedBy(paths.resolve, lane, override)
}

// codexElectedNameFn reports which lane Codex's election named, "" for none,
// tracing a change the way electedNameFn does.
func codexElectedNameFn(paths codexElectionPaths, lane activation.Lane, override *bool) func() string {
	return electedNameBy(paths.resolve, lane, "codex:"+string(lane), override)
}

// codexElectionProblemFn reports why Codex's election could not be decided.
func codexElectionProblemFn(paths codexElectionPaths, override *bool) func() string {
	return electionProblemBy(paths.resolve, override)
}

// codexObserver is what the relay calls when a model call carrying Codex's
// carrier crosses it: it writes the evidence Codex's election needs before
// the relay may outrank telemetry. A failure to write it costs only the
// election staying with telemetry, so it is reported (at most hourly) and
// never raised.
func codexObserver(paths codexElectionPaths, logger *log.Logger) func() {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	var mu sync.Mutex
	var lastWarn time.Time
	return func() {
		wrote, err := activation.MarkCodexProxyObserved(paths.pacRecord, paths.marker)
		if err != nil {
			mu.Lock()
			due := time.Since(lastWarn) >= time.Hour
			if due {
				lastWarn = time.Now()
			}
			mu.Unlock()
			if due {
				logger.Printf("openbox transport: could not record that the relay has seen Codex (%v); "+
					"Codex's model calls stay with the telemetry lane until it can", err)
			}
			return
		}
		if wrote {
			logger.Printf("openbox transport: the relay has now seen a Codex model call; it is the " +
				"elected producer of Codex model-call turns and telemetry stands by")
		}
	}
}

// reportCodexElection logs a daemon's startup view of Codex's election. lane
// is the lane the daemon speaks for ("telemetry" or "transport").
func reportCodexElection(logger *log.Logger, paths codexElectionPaths, lane activation.Lane, override bool) {
	e := paths.resolve()
	name := string(lane)
	if override {
		logger.Printf("openbox %s: emitting Codex model-call turns because --elected was passed, "+
			"overriding the election (which currently names %q)", name, orNone(string(e.Elected)))
		return
	}
	if problem := e.SettingsProblem; problem != "" {
		logger.Printf("openbox %s: CANNOT DECIDE whether to emit Codex model-call turns: %s. "+
			"Reinstall with `openbox init --provider codex` so the unit carries --codex-settings, or "+
			"pass --elected. Claude Code's own election is unaffected.", name, problem)
		return
	}
	if e.Elected == lane {
		logger.Printf("openbox %s: elected producer of Codex model-call turns; %s", name, e.Reason)
		return
	}
	logger.Printf("openbox %s: NOT the elected producer of Codex model-call turns (%s); "+
		"emitting none. Re-checked per call.", name, e.Reason)
}

func reportElection(logger *log.Logger, lane string, settingsPath string, want activation.Lane, override bool) {
	e := activation.ResolveElection(settingsPath)
	if override {
		logger.Printf("openbox %s: emitting model-call turns because --elected was passed, "+
			"overriding the election (which currently names %q)", lane, orNone(string(e.Elected)))
		return
	}
	if problem := e.SettingsProblem; problem != "" {
		logger.Printf("openbox %s: CANNOT DECIDE whether to emit model-call turns: %s. "+
			"This is not the same as no lane being routed, and it is what a lane reading a path "+
			"it cannot reach looks like. Reinstall with `openbox init` so the unit carries the "+
			"path, or pass --settings; `--elected` forces this lane to emit meanwhile.",
			lane, problem)
		return
	}
	switch {
	case e.Elected == want:
		logger.Printf("openbox %s: elected producer of model-call turns; %s", lane, e.Reason)
	default:
		logger.Printf("openbox %s: NOT the elected producer (%s is; %s); emitting no model-call "+
			"turns. Re-checked per call, so this changes on its own once the tool's settings are "+
			"written -- install starts this daemon before the env var exists.",
			lane, orNone(string(e.Elected)), e.Reason)
	}
}

// orNone renders an empty coordinate as something a reader can distinguish
// from a value. It lived in init.go, which went with the approver persona.
func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
