package claudecode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Nothing bound them together, so a hook registered by the installer but not
// known to the engine would be installed, fire, and be rejected as "unknown
// Claude Code hook"; and one known to the engine but never registered would
// simply never fire.
//
// These used to compare the engine against an embedded plugin manifest. That
// manifest is gone -- a plugin's copy of these handlers does not de-duplicate
// against the settings-level registrations, so anything that loaded it doubled
// every event -- and the binding that matters is between what writeHooks
// registers and what the engine dispatches.

type registeredHooksJSON struct {
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Type        string `json:"type"`
			Command     string `json:"command"`
			Timeout     int    `json:"timeout"`
			AsyncRewake bool   `json:"asyncRewake"`
			// Async is a pointer: absent and false must never collapse into one
			// state. A plain bool cannot tell "the key was not
			// written" from "the key was written false".
			Async *bool `json:"async"`
		} `json:"hooks"`
	} `json:"hooks"`
}

// registeredHooks is what an install actually writes, parsed back.
const testEngine = "/opt/openbox/bin/openbox"

func registeredHooks(t *testing.T) registeredHooksJSON {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".claude", "settings.json")
	if err := writeHooks(path, testEngine); err != nil {
		t.Fatalf("writeHooks: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the written settings: %v", err)
	}
	var parsed registeredHooksJSON
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse the written settings: %v", err)
	}
	return parsed
}

func TestRegisteredHooksMatchEngineVocabulary(t *testing.T) {
	parsed := registeredHooks(t)

	fromSettings := make([]string, 0, len(parsed.Hooks))
	for name := range parsed.Hooks {
		fromSettings = append(fromSettings, name)
	}
	fromEngine := make([]string, 0, len(hookNames))
	for name := range hookNames {
		fromEngine = append(fromEngine, string(name))
	}
	assertSameHookSet(t, "the registered settings", fromSettings, "engine hookNames", fromEngine)

	for name := range parsed.Hooks {
		if _, err := ParseHookName(name); err != nil {
			t.Errorf("registered hook %q is not dispatchable: %v", name, err)
		}
	}
}

// TestRegisteredTimeoutsMatchTheEventTable. localHookEvents is the one
// declaration of each event's budget; a written timeout that disagreed with it
// would be a budget nothing in the engine derives from.
func TestRegisteredTimeoutsMatchTheEventTable(t *testing.T) {
	parsed := registeredHooks(t)
	byName := map[string]int{}
	for _, ev := range localHookEvents {
		byName[ev.Event] = ev.Timeout
	}
	for name, entries := range parsed.Hooks {
		if len(entries) == 0 || len(entries[0].Hooks) == 0 {
			t.Errorf("registered hook %q has no handler", name)
			continue
		}
		if got, want := byName[name], entries[0].Hooks[0].Timeout; got != want {
			t.Errorf("hook %q timeout: localHookEvents=%d, written=%d", name, got, want)
		}
	}
}

// TestTurnHooksAreWiredAsNonGating the turn-boundary hooks are wired with the
// ordinary non-gating budget and no matcher.
func TestTurnHooksAreWiredAsNonGating(t *testing.T) {
	parsed := registeredHooks(t)

	for _, name := range []string{"Stop", "SubagentStop"} {
		entries, ok := parsed.Hooks[name]
		if !ok {
			t.Errorf("the install does not register %s; per-turn usage would never be collected", name)
			continue
		}
		if len(entries) != 1 || len(entries[0].Hooks) != 1 {
			t.Errorf("%s should wire exactly one handler, got %+v", name, entries)
			continue
		}
		h := entries[0].Hooks[0]
		if entries[0].Matcher != "" {
			t.Errorf("%s matcher = %q, want empty (there is no tool to match)", name, entries[0].Matcher)
		}
		if h.Timeout != 5 {
			t.Errorf("%s timeout = %d, want 5 (it never holds for anything)", name, h.Timeout)
		}
		if h.AsyncRewake {
			t.Errorf("%s must not be an async rewake handler", name)
		}
		want := `"` + testEngine + `" hook claude-code ` + name
		if h.Command != want {
			t.Errorf("%s command = %q, want %q", name, h.Command, want)
		}
	}
}

// TestAsyncRowsWriteTheAsyncKey is derived by iteration over localHookEvents,
// never a hand-written list: every row with Async: true must write
// "async": true, and every row with Async: false must write no async key at
// all. ("observe-only" and "async" are not the same set, so this test is not
// phrased in terms of observe-only-ness.)
func TestAsyncRowsWriteTheAsyncKey(t *testing.T) {
	parsed := registeredHooks(t)
	for _, ev := range localHookEvents {
		entries, ok := parsed.Hooks[ev.Event]
		if !ok || len(entries) == 0 || len(entries[0].Hooks) == 0 {
			t.Errorf("%s: no handler registered", ev.Event)
			continue
		}
		h := entries[0].Hooks[0]
		switch {
		case ev.Async && (h.Async == nil || !*h.Async):
			t.Errorf("%s: localHookEvents says Async:true but the written async key is %v, want true", ev.Event, h.Async)
		case !ev.Async && h.Async != nil:
			t.Errorf("%s: localHookEvents says Async:false but the written async key is %v, want absent", ev.Event, *h.Async)
		}
	}
}

// TestHeadlineSyncSetIsExact is the exhaustive closed-set guard
// TestGatingHooksAreNeverAsync alone cannot be (that test names
// only the three gating hooks, so flipping PermissionRequest to async would
// pass it silently). An async hook is killed at `claude -p` teardown, so
// demoting one of these 17 to async would silently lose a human-decision
// event in headless runs.
func TestHeadlineSyncSetIsExact(t *testing.T) {
	want := map[string]bool{
		"PreToolUse": true, "UserPromptSubmit": true, "SessionStart": true,
		"PostToolUse": true, "PostToolUseFailure": true, "Stop": true,
		"SubagentStop": true, "SubagentStart": true, "PermissionDenied": true,
		"StopFailure": true, "SessionEnd": true, "ConfigChange": true,
		"PermissionRequest": true, "TaskCreated": true, "TaskCompleted": true,
		"Elicitation": true, "ElicitationResult": true,
	}
	if len(want) != 17 {
		t.Fatalf("test fixture has %d entries, want 17", len(want))
	}
	parsed := registeredHooks(t)
	got := map[string]bool{}
	for name, entries := range parsed.Hooks {
		if len(entries) == 0 || len(entries[0].Hooks) == 0 {
			continue
		}
		if entries[0].Hooks[0].Async == nil {
			got[name] = true
		}
	}
	assertSameHookSet(t, "the sync (async-key-absent) set", setToSlice(got), "the declared 17-row headline sync set", setToSlice(want))
}

func setToSlice(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestAsyncIsAbsentNotFalse no written handler may carry "async": false;
// absent and false must not be two states for the same fact.
func TestAsyncIsAbsentNotFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude", "settings.json")
	if err := writeHooks(path, testEngine); err != nil {
		t.Fatalf("writeHooks: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the written settings: %v", err)
	}
	if bytes.Contains(raw, []byte(`"async":false`)) || bytes.Contains(raw, []byte(`"async": false`)) {
		t.Errorf("the written settings contain a literal \"async\": false; absent and false must not be two states:\n%s", raw)
	}
}

// TestGatingHooksAreNeverAsync the three hooks that can return a blocking
// verdict (PreToolUse, UserPromptSubmit, and ConfigChange) can never
// be registered async: an async hook cannot hold a tool call open while core
// decides. Not a sufficient guard on its own — see
// TestHeadlineSyncSetIsExact for the exhaustive form.
func TestGatingHooksAreNeverAsync(t *testing.T) {
	parsed := registeredHooks(t)
	for _, name := range []string{"PreToolUse", "UserPromptSubmit", "ConfigChange"} {
		entries, ok := parsed.Hooks[name]
		if !ok || len(entries) == 0 || len(entries[0].Hooks) == 0 {
			t.Errorf("%s: no handler registered", name)
			continue
		}
		if h := entries[0].Hooks[0]; h.Async != nil {
			t.Errorf("%s: the GATE must be synchronous; an async handler cannot block a tool call, got async=%v", name, *h.Async)
		}
	}
}

func assertSameHookSet(t *testing.T, aName string, a []string, bName string, b []string) {
	t.Helper()
	index := func(list []string) map[string]bool {
		m := make(map[string]bool, len(list))
		for _, s := range list {
			m[s] = true
		}
		return m
	}
	inA, inB := index(a), index(b)
	var onlyA, onlyB []string
	for _, s := range a {
		if !inB[s] {
			onlyA = append(onlyA, s)
		}
	}
	for _, s := range b {
		if !inA[s] {
			onlyB = append(onlyB, s)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	if len(onlyA) > 0 {
		t.Errorf("in %s but not %s: %v", aName, bName, onlyA)
	}
	if len(onlyB) > 0 {
		t.Errorf("in %s but not %s: %v", bName, aName, onlyB)
	}
}
