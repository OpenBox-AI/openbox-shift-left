package claudecode

import "testing"

func TestCapabilities(t *testing.T) {
	caps := Capabilities()
	byKey := map[string]Capability{}
	for _, c := range caps {
		if _, dup := byKey[c.Key]; dup {
			t.Errorf("duplicate capability key %q", c.Key)
		}
		if c.How == "" {
			t.Errorf("capability %q missing a How note", c.Key)
		}
		byKey[c.Key] = c
	}

	// Provider-independent floors + the Claude Code surfaces are supported.
	for _, k := range []string{"identity.register", "telemetry.hook", "tool.events", "commit.binding"} {
		if !byKey[k].Supported {
			t.Errorf("capability %q should be supported", k)
		}
	}
	// Phase-1 observe: enforcement is declared-but-inactive; tokens unavailable.
	if byKey["verdict.apply"].Supported {
		t.Error("verdict.apply must be false in Phase-1 observe (D7/INV-3)")
	}
	if byKey["telemetry.tokens"].Supported {
		t.Error("telemetry.tokens is not available from Claude Code hooks")
	}
}
