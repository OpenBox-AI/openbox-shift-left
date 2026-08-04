package claudecode

import (
	"strings"
	"testing"

	providerspi "github.com/openbox-ai/openbox-shift-left/provider"
)

func TestCapabilities(t *testing.T) {
	caps := Capabilities()
	byKey := map[string]providerspi.Capability{}
	for _, c := range caps {
		if _, dup := byKey[c.Key]; dup {
			t.Errorf("duplicate capability key %q", c.Key)
		}
		if c.How == "" {
			t.Errorf("capability %q missing a How note", c.Key)
		}
		byKey[c.Key] = c
	}

	// Provider-independent floors, the Claude Code surfaces, and the shipped
	// opt-in legs (E6 enforce, SL-16 finops) are all supported.
	for _, k := range []string{
		"identity.register", "telemetry.hook", "tool.events", "commit.binding",
		"telemetry.tokens", "verdict.apply", "enforce.rewrite",
	} {
		if !byKey[k].Supported {
			t.Errorf("capability %q should be supported", k)
		}
	}
	// The opt-in legs must say so: "supported" is about the mechanism existing,
	// and a reader of the profile has to be able to tell that an unconfigured
	// session still only observes (INV-3, report SL-07).
	for _, k := range []string{"telemetry.tokens", "verdict.apply"} {
		if !strings.Contains(byKey[k].How, "opt-in") {
			t.Errorf("capability %q is opt-in; its How note must say so, got %q", k, byKey[k].How)
		}
	}
}
