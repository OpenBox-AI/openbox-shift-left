package main

import (
	"log"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayservice"
)

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
	return func() string {
		if override != nil && *override {
			return string(lane) // forced by the operator; this lane is the producer
		}
		return string(activation.ResolveElection(settingsPath).Elected)
	}
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
