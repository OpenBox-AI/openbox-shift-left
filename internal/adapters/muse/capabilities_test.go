package muse

import (
	"strings"
	"testing"
)

func TestCapabilitiesAreHonest(t *testing.T) {
	got := map[string]bool{}
	notes := map[string]string{}
	for _, c := range Capabilities() {
		if c.Key == "" || c.How == "" {
			t.Errorf("capability %+v needs a key and a note", c)
		}
		if _, dup := got[c.Key]; dup {
			t.Errorf("duplicate capability %q", c.Key)
		}
		got[c.Key] = c.Supported
		notes[c.Key] = c.How
	}
	for _, key := range []string{"identity.register", "telemetry.hook", "tool.events", "tool.status", "verdict.apply", "model_call_gate", "enforce.rewrite"} {
		if !got[key] {
			t.Errorf("%s should be supported", key)
		}
	}
	// No false coverage: this adapter records no model call and reports no
	// tokens or commit binding, and says so as a declared gap.
	for _, key := range []string{"model_call.record", "telemetry.tokens", "commit.binding"} {
		supported, declared := got[key]
		if !declared || supported {
			t.Errorf("%s must be declared unsupported (declared=%v supported=%v)", key, declared, supported)
		}
		if !strings.Contains(notes[key], "NOT REPORTED") {
			t.Errorf("%s: the gap is not stated: %q", key, notes[key])
		}
	}
	for _, c := range Capabilities() {
		if strings.Contains(c.Key, "telemetry.model") || strings.Contains(c.Key, "finops") {
			t.Errorf("capability %q claims coverage the adapter does not have", c.Key)
		}
	}
	if !strings.Contains(notes["verdict.apply"], "not a tamper-proof boundary") {
		t.Error("verdict.apply must not imply tamper resistance")
	}
	if !strings.Contains(notes["telemetry.hook"], "unverified") {
		t.Error("telemetry.hook must state that the payload shapes are unverified on a binary")
	}
}

func TestHookCeilings(t *testing.T) {
	c := Engine{}.HookCeilings()
	if c.Gating.Seconds() != 30 || c.Other.Seconds() != 5 {
		t.Errorf("ceilings = %+v, want Gating 30s and Other 5s", c)
	}
	if evaluator.Ceiling != c {
		t.Errorf("the evaluator's ceiling %+v is not the declared one %+v", evaluator.Ceiling, c)
	}
	if inlineWindow+1e9 > c.Other || inlineAttemptTimeout >= inlineWindow {
		t.Errorf("inline window %v / attempt %v do not fit the %v ceiling", inlineWindow, inlineAttemptTimeout, c.Other)
	}
}
