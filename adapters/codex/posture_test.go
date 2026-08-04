package codex

import (
	"testing"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
)

// Posture is session-scoped evidence, so it belongs on the session-start event
// and nowhere else: repeating it per tool call would inflate every event and
// invite two events in one session to disagree.
func TestPosture_OnSessionStartOnly(t *testing.T) {
	m := testMapper()
	p := devconfig.Posture{Enforce: true, Staleness: devconfig.StalenessFresh, Adapter: "codex/1"}
	m.Posture = &p

	start, ok := m.Map(HookSessionStart, &HookEvent{SessionID: "s1", Source: "startup"})
	if !ok {
		t.Fatal("SessionStart did not map")
	}
	got, present := start.Metadata["posture"].(map[string]any)
	if !present {
		t.Fatalf("SessionStarted carries no posture: %v", start.Metadata)
	}
	if got["enforce"] != true || got["staleness"] != string(devconfig.StalenessFresh) {
		t.Errorf("posture did not round-trip: %v", got)
	}

	for _, hook := range []HookName{
		HookUserPromptSubmit, HookPreToolUse, HookPostToolUse, HookSessionEnd,
	} {
		ev, ok := m.Map(hook, &HookEvent{SessionID: "s1", ToolName: "Bash"})
		if !ok {
			t.Fatalf("%s did not map", hook)
		}
		if _, present := ev.Metadata["posture"]; present {
			t.Errorf("%s must not carry posture (session-scoped evidence)", hook)
		}
	}
}

// With no posture supplied — every existing test, the conformance fixtures, and
// any caller that has not opted in — the emitted metadata is unchanged.
func TestPosture_AbsentWhenNotSupplied(t *testing.T) {
	m := testMapper() // Posture stays nil
	ev, _ := m.Map(HookSessionStart, &HookEvent{SessionID: "s1", Source: "startup"})
	if _, present := ev.Metadata["posture"]; present {
		t.Errorf("no posture should be emitted when none was resolved: %v", ev.Metadata)
	}
}

// The staleness enum must distinguish *why* a check did not happen — that
// distinction is the SL-03 fix, and collapsing it back to a bare "skipped"
// would silently undo the story.
func TestPosture_StalenessNamesTheSkipReason(t *testing.T) {
	distinct := map[devconfig.Staleness]bool{}
	for _, s := range []devconfig.Staleness{
		devconfig.StalenessNotChecked,
		devconfig.StalenessFresh,
		devconfig.StalenessStaleWarned,
		devconfig.StalenessStaleBlocked,
		devconfig.StalenessSkippedNoToken,
		devconfig.StalenessSkippedNoPin,
		devconfig.StalenessError,
	} {
		if s == "" {
			t.Error("staleness values must be non-empty so they render in metadata")
		}
		if distinct[s] {
			t.Errorf("duplicate staleness value %q — the outcomes must be distinguishable", s)
		}
		distinct[s] = true
	}
}

// codexMandated must key on a TOP-LEVEL requirement key. The E8-S8 template
// listed the mandate keys under a `[hooks]` header, so Codex bound them as
// `hooks.*` and ignored them — while a `hook codex` substring check still let
// posture report provider_managed:true. Reporting a mandate that is not in effect
// is worse than reporting none, because the whole point of the field is that the
// control plane can stop taking assurance on faith.
func TestCodexMandated(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"top-level pin", "allowed_sandbox_modes = [\"read-only\"]\n", true},
		{"top-level exclusivity", "allow_managed_hooks_only = true\n", true},
		{"pins above a table", "allowed_approval_policies = [\"untrusted\"]\n\n[experimental_network]\nenabled = true\n", true},
		{"nested under [hooks] — inert", "[hooks]\nallow_managed_hooks_only = true\nallowed_sandbox_modes = [\"read-only\"]\n", false},
		{"names our hook but mandates nothing", "[hooks]\nPreToolUse = \"openbox hook codex PreToolUse\"\n", false},
		{"commented out", "# allowed_sandbox_modes = [\"read-only\"]\n", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := codexMandated([]byte(c.body)); got != c.want {
			t.Errorf("%s: codexMandated = %v, want %v", c.name, got, c.want)
		}
	}
}
