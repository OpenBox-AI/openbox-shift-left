package codex

import (
	"strings"
	"testing"
)

// The declared capability profile is the §1b coverage contract for this
// provider: pin the keys and the truth (telemetry.tokens true as of the SL7-C
// finops leg; verdict.apply + enforce.rewrite true as of the SL7-B enforce leg)
// so a silent flip fails loudly here.
func TestCapabilitiesProfile(t *testing.T) {
	want := map[string]bool{
		"identity.register": true,
		"telemetry.hook":    true,
		"tool.events":       true,
		"commit.binding":    true,
		"telemetry.tokens":  true, // rollout-JSONL finops extraction; per SESSION (ADR-0014)
		"telemetry.model":   true, // session-level model id from turn_context (ADR-0014)
		"verdict.apply":     true, // STORY-SL7-B: PreToolUse deny gate (opt-in)
		"enforce.rewrite":   true, // STORY-SL7-B: local secret redaction via allow+updatedInput
		// ADR-0018: Claude Code reports per-tool success, Codex does not. Pinned
		// FALSE so the divergence is a declared limit rather than a gap someone
		// closes later with a heuristic over tool_response.
		"tool.status": false,
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

	// Codex's granularity limit must be stated as SCOPE, never as impossibility —
	// its Stop hook exists in v0.145.0 and is deliberately unwired. A profile that
	// implied "Codex cannot do per-turn" would be the exact self-overstatement this
	// repo exists to prevent, in the direction of understating a competitor
	// runtime's capability.
	tokens := byKey["telemetry.tokens"]
	for _, want := range []string{"PER SESSION", "NOT per turn", "Stop hook exists", "unwired"} {
		if !strings.Contains(tokens, want) {
			t.Errorf("telemetry.tokens How note must record the granularity limit as scope (%q missing): %q", want, tokens)
		}
	}
	for _, forbidden := range []string{"cannot", "impossible", "not possible"} {
		if strings.Contains(strings.ToLower(tokens), forbidden) {
			t.Errorf("telemetry.tokens How note implies impossibility (%q); the limit is scope: %q", forbidden, tokens)
		}
	}
	// And it must say which way it is gated. Usage capture is opt-OUT as of the
	// finops default flip, so a reader must not infer that an unconfigured session
	// stays silent.
	if !strings.Contains(tokens, "default on") {
		t.Errorf("telemetry.tokens is on by default; the How note must say so: %q", tokens)
	}

	// The same discipline for the outcome gap: unsupported must read as "the
	// payload carries no structural outcome", never as "assume success". A
	// future edit that made this note vague would be the first step toward
	// shipping status:"completed" unconditionally, which reports 100% success
	// for a session whose calls failed.
	status := byKey["tool.status"]
	for _, want := range []string{"no failure hook", "no exit code", "SUCCESS 100%"} {
		if !strings.Contains(status, want) {
			t.Errorf("tool.status How note must say why it is unreported (%q missing): %q", want, status)
		}
	}
}
