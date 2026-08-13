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
	// Both are opt-OUT now, and the profile has to say so. verdict.apply read
	// "opt-in, default observe" long after ADR-0016 flipped enforce ON — a note
	// telling a reader an unconfigured session cannot block, when it can. That is
	// the exact failure this check exists to catch, so it now asserts the
	// direction rather than a fixed word.
	for _, k := range []string{"verdict.apply"} {
		if !strings.Contains(byKey[k].How, "default") {
			t.Errorf("capability %q must state its default, so a reader knows what an "+
				"unconfigured session does; got %q", k, byKey[k].How)
		}
		if strings.Contains(byKey[k].How, "opt-in") {
			t.Errorf("capability %q claims opt-in; enforce is ON by default (ADR-0016), and a "+
				"profile that understates what a session does is as misleading as one that "+
				"oversells it; got %q", k, byKey[k].How)
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
