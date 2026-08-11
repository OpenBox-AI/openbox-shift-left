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
	// gated legs (E6 enforce opt-in, ADR-0014 usage capture opt-out) are supported.
	for _, k := range []string{
		"identity.register", "telemetry.hook", "tool.events", "commit.binding",
		"telemetry.tokens", "verdict.apply", "enforce.rewrite",
	} {
		if !byKey[k].Supported {
			t.Errorf("capability %q should be supported", k)
		}
	}
	// Every gated leg must state which way it is gated: "supported" is about the
	// mechanism existing, and a reader of the profile has to be able to tell what
	// an unconfigured session actually does (INV-3, report SL-07).
	//
	// The two are gated in OPPOSITE directions, and conflating them is exactly the
	// misreading this asserts against: enforcement is opt-IN (an unconfigured
	// session only observes and can never block), while usage capture is opt-OUT
	// as of ADR-0014 / the finops default flip (an unconfigured session DOES emit
	// token counts and a model id).
	for _, k := range []string{"verdict.apply"} {
		if !strings.Contains(byKey[k].How, "opt-in") {
			t.Errorf("capability %q is opt-in; its How note must say so, got %q", k, byKey[k].How)
		}
	}
	if how := byKey["telemetry.tokens"].How; !strings.Contains(how, "default on") {
		t.Errorf("telemetry.tokens is on by default; its How note must say so (a reader who assumes opt-in "+
			"would not know data egresses unconfigured), got %q", how)
	}
	if how := byKey["telemetry.tokens"].How; strings.Contains(how, "opt-in") {
		t.Errorf("telemetry.tokens is no longer opt-in; the How note still claims it is: %q", how)
	}
}
