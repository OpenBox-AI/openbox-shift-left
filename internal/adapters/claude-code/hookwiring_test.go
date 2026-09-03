package claudecode

import (
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
