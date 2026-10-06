package codex

import (
	"strings"
	"testing"
)

// TestCapabilitiesProfile the declared capability profile is the coverage
// contract for this provider: pin the keys and the truth (telemetry.tokens
// true with the finops leg; verdict.apply + enforce.rewrite true with the
// enforce leg) so a silent flip fails loudly here.
func TestCapabilitiesProfile(t *testing.T) {
	want := map[string]bool{
		"identity.register": true,
		"telemetry.hook":    true,
		"tool.events":       true,
		"commit.binding":    true,
		"telemetry.tokens":  true, // per-TURN delta via Stop; rollup only when no turn fired
		"telemetry.model":   true, // per-turn model id from the window's turn_context
		"verdict.apply":     true, // three gate classes + the session-halt latch
		"enforce.rewrite":   true, // local secret redaction via allow+updatedInput
		"tool.status":       false,
	}
	got := Capabilities()
	if len(got) != len(want) {
		t.Fatalf("capability count = %d, want %d", len(got), len(want))
	}
	byKey := map[string]string{}
	for _, c := range got {
		supported, known := want[c.Key]
		if !known {
			t.Errorf("unexpected capability key %q", c.Key)
			continue
		}
		if c.Supported != supported {
			t.Errorf("capability %q supported = %t, want %t", c.Key, c.Supported, supported)
		}
		if c.How == "" {
			t.Errorf("capability %q missing its How note", c.Key)
		}
		byKey[c.Key] = c.How
	}

	// Stop is wired, so usage is per turn: per-turn is asserted, the rollup's
	// surviving role is stated, and the no-double-count rule (the rollup ships
	// only when no turn fired) is visible in the note.
	tokens := byKey["telemetry.tokens"]
	for _, want := range []string{"PER TURN", "DELTA", "zero turns", "SUB-counts"} {
		if !strings.Contains(tokens, want) {
			t.Errorf("telemetry.tokens How note must record the per-turn contract (%q missing): %q", want, tokens)
		}
	}
	// Every live claim must be traceable. A note that says "measured" without
	// naming what holds it is a claim nothing verifies.
	for key, note := range byKey {
		if !strings.Contains(note, "Test") &&
			!strings.Contains(note, ".go") && !strings.Contains(note, "provider-independent") {
			t.Errorf("capability %q cites neither a test name nor a source file: %q", key, note)
		}
	}
	for _, forbidden := range []string{"cannot", "impossible", "not possible"} {
		if strings.Contains(strings.ToLower(tokens), forbidden) {
			t.Errorf("telemetry.tokens How note implies impossibility (%q); the limit is scope: %q", forbidden, tokens)
		}
	}
	// Usage capture is opt-OUT as of the finops default flip, so a reader must
	// not infer that an unconfigured session stays silent.
	if !strings.Contains(tokens, "default on") {
		t.Errorf("telemetry.tokens is on by default; the How note must say so: %q", tokens)
	}

	// The same anti-overstatement rule applies to the gate note, which is the one
	// most likely to drift into implying total coverage.
	apply := byKey["verdict.apply"]
	for _, forbidden := range []string{"cannot", "impossible", "not possible"} {
		if strings.Contains(strings.ToLower(apply), forbidden) {
			t.Errorf("verdict.apply How note implies impossibility (%q): %q", forbidden, apply)
		}
	}
	for _, want := range []string{"not a complete enforcement boundary", "hosted tools", "write_stdin"} {
		if !strings.Contains(apply, want) {
			t.Errorf("verdict.apply must state its limits (%q missing); an enforcement claim without "+
				"them overstates coverage: %q", want, apply)
		}
	}

	status := byKey["tool.status"]
	for _, want := range []string{"no failure hook", "no exit code", "SUCCESS 100%"} {
		if !strings.Contains(status, want) {
			t.Errorf("tool.status How note must say why it is unreported (%q missing): %q", want, status)
		}
	}
}
