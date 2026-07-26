package codex

import "testing"

// The declared capability profile is the §1b coverage contract for this
// provider: pin the keys and the truth (no tokens yet; verdict.apply +
// enforce.rewrite true as of the SL7-B enforce leg) so a silent flip fails
// loudly here.
func TestCapabilitiesProfile(t *testing.T) {
	want := map[string]bool{
		"identity.register": true,
		"telemetry.hook":    true,
		"tool.events":       true,
		"commit.binding":    true,
		"telemetry.tokens":  false,
		"verdict.apply":     true, // STORY-SL7-B: PreToolUse deny gate (opt-in)
		"enforce.rewrite":   true, // STORY-SL7-B: Tier-1 secret redaction via allow+updatedInput
	}
	got := Capabilities()
	if len(got) != len(want) {
		t.Fatalf("capability count = %d, want %d", len(got), len(want))
	}
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
	}
}
