package claudecode

import (
	"testing"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
)

// Posture is session-scoped evidence, so it belongs on the session-start event
// and nowhere else: repeating it per tool call would inflate every event and
// invite two events in one session to disagree.
func TestPosture_OnSessionStartOnly(t *testing.T) {
	m := testMapper()
	p := devconfig.Posture{Enforce: true, Staleness: devconfig.StalenessFresh, Adapter: "claude-code/1"}
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
