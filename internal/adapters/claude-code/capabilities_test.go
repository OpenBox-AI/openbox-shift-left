package claudecode

import (
	"strings"
	"testing"

	providerspi "github.com/openbox-ai/openbox-shift-left/internal/provider"
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

	for _, k := range []string{
		"identity.register", "telemetry.hook", "tool.events", "commit.binding",
		"telemetry.tokens", "verdict.apply", "enforce.rewrite",
		// Pinned true here and false in the Codex profile: the two providers
		// genuinely diverge, and the divergence must be declared on both sides
		// rather than discovered from an empty dashboard panel.
		"tool.status",
	} {
		if !byKey[k].Supported {
			t.Errorf("capability %q should be supported", k)
		}
	}
	// Both are opt-OUT now, and the profile has to say so. Verdict.apply read
	// "opt-in, default observe" long after that decision flipped enforce ON; a
	// note telling a reader an unconfigured session cannot block, when it can.
	for _, k := range []string{"verdict.apply"} {
		if !strings.Contains(byKey[k].How, "default") {
			t.Errorf("capability %q must state its default, so a reader knows what an "+
				"unconfigured session does; got %q", k, byKey[k].How)
		}
		if strings.Contains(byKey[k].How, "opt-in") {
			t.Errorf("capability %q claims opt-in; enforce is ON by default, and a "+
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
