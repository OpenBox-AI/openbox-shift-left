package main

import (
	"log"
	"sync"

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
	return func() bool {
		if override != nil && *override {
			return true
		}
		return activation.ResolveElection(settingsPath).Elected == lane
	}
}

// electedNameFn reports WHICH lane the election named, "" for none.
//
// The emitter needs this to tell "another lane won" -- healthy, and quiet -- from
// "the settings route no lane at all", where nothing emits and a relay holding a
// call is a routing gap worth saying out loud.
func electedNameFn(settingsPath string, lane activation.Lane, override *bool) func() string {
	var mu sync.Mutex
	last := electionChangeSentinel
	return func() string {
		var name string
		if override != nil && *override {
			name = string(lane) // forced by the operator; this lane is the producer
		} else {
			name = string(activation.ResolveElection(settingsPath).Elected)
		}
		traceElectionChange(&mu, &last, string(lane), name)
		return name
	}
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
	return func() string {
		if override != nil && *override {
			return "" // decided by the operator; nothing to read
		}
		return activation.ResolveElection(settingsPath).SettingsProblem
	}
}

// codexElectedFn is electedFn's Codex counterpart: it reads config.toml
// through activation.ResolveCodexElection, never settings.json through
// ResolveElection -- the two native surfaces must never be read through each
// other's parser, or a TOML file handed to the JSON reader reports "cannot
// decide" forever.
func codexElectedFn(configPath string, override *bool) func() bool {
	return func() bool {
		if override != nil && *override {
			return true
		}
		return activation.ResolveCodexElection(configPath).Elected == activation.LaneTelemetry
	}
}

func reportCodexElection(logger *log.Logger, configPath string, override bool) {
	e := activation.ResolveCodexElection(configPath)
	if override {
		logger.Printf("openbox telemetry: emitting Codex model-call turns because --elected was passed, "+
			"overriding the election (which currently names %q)", orNone(string(e.Elected)))
		return
	}
	if problem := e.SettingsProblem; problem != "" {
		logger.Printf("openbox telemetry: CANNOT DECIDE whether to emit Codex model-call turns: %s. "+
			"Reinstall with `openbox init --provider codex` so the unit carries --codex-settings, or "+
			"pass --elected. Claude Code's own election is unaffected.", problem)
		return
	}
	if e.Elected == activation.LaneTelemetry {
		logger.Printf("openbox telemetry: elected producer of Codex model-call turns; %s", e.Reason)
		return
	}
	logger.Printf("openbox telemetry: NOT the elected producer of Codex model-call turns (%s); "+
		"emitting none. Re-checked per call.", e.Reason)
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
