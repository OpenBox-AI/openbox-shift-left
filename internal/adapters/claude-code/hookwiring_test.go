package claudecode

import (
	"encoding/json"
	"sort"
	"testing"
)

// Nothing bound them together, so a hook added to the bundle but not to the
// engine would be installed, fire, and be rejected as "unknown Claude Code
// hook"; and one added to the engine but not the bundle would simply never
// fire.

type pluginHooksJSON struct {
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

func loadPluginHooks(t *testing.T) pluginHooksJSON {
	t.Helper()
	raw, err := pluginFS.ReadFile("plugin/hooks/hooks.json")
	if err != nil {
		t.Fatalf("read embedded hooks.json: %v", err)
	}
	var parsed pluginHooksJSON
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse hooks.json: %v", err)
	}
	return parsed
}

func TestPluginHooksMatchEngineVocabulary(t *testing.T) {
	parsed := loadPluginHooks(t)

	fromBundle := make([]string, 0, len(parsed.Hooks))
	for name := range parsed.Hooks {
		fromBundle = append(fromBundle, name)
	}
	fromEngine := make([]string, 0, len(hookNames))
	for name := range hookNames {
		fromEngine = append(fromEngine, string(name))
	}
	assertSameHookSet(t, "plugin hooks.json", fromBundle, "engine hookNames", fromEngine)

	for name := range parsed.Hooks {
		if _, err := ParseHookName(name); err != nil {
			t.Errorf("bundled hook %q is not dispatchable: %v", name, err)
		}
	}
}

func TestLocalHooksMirrorPluginBundle(t *testing.T) {
	parsed := loadPluginHooks(t)

	fromLocal := make([]string, 0, len(localHookEvents))
	byName := map[string]int{}
	for _, ev := range localHookEvents {
		fromLocal = append(fromLocal, ev.Event)
		byName[ev.Event] = ev.Timeout
	}
	fromBundle := make([]string, 0, len(parsed.Hooks))
	for name := range parsed.Hooks {
		fromBundle = append(fromBundle, name)
	}
	assertSameHookSet(t, "localHookEvents", fromLocal, "plugin hooks.json", fromBundle)

	for name, entries := range parsed.Hooks {
		if len(entries) == 0 || len(entries[0].Hooks) == 0 {
			t.Errorf("bundled hook %q has no handler", name)
			continue
		}
		if got, want := byName[name], entries[0].Hooks[0].Timeout; got != want {
			t.Errorf("hook %q timeout: localHookEvents=%d, hooks.json=%d", name, got, want)
		}
	}
}

// TestTurnHooksAreWiredAsNonGating the turn-boundary hooks are wired with the
// ordinary non-gating budget and no matcher.
func TestTurnHooksAreWiredAsNonGating(t *testing.T) {
	parsed := loadPluginHooks(t)

	for _, name := range []string{"Stop", "SubagentStop"} {
		entries, ok := parsed.Hooks[name]
		if !ok {
			t.Errorf("hooks.json does not wire %s; per-turn usage would never be collected", name)
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
		want := "\"${CLAUDE_PLUGIN_ROOT}/bin/openbox\" hook claude-code " + name
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
