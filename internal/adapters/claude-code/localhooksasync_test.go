package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// asyncFixture is what writeHooks actually writes, parsed back, keyed by
// event name -> first handler's fields. Only the handler this adapter owns
// (matcher-group 0, hook 0) matters here; PreToolUse's second (rewake) handler
// is asserted separately where it matters.
type asyncFixture struct {
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Command string `json:"command"`
			Timeout int    `json:"timeout"`
			Async   *bool  `json:"async"`
		} `json:"hooks"`
	} `json:"hooks"`
}

func writeAndParseHooks(t *testing.T, engine string) asyncFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".claude", "settings.json")
	if err := writeHooks(path, engine); err != nil {
		t.Fatalf("writeHooks: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var parsed asyncFixture
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse settings: %v\n%s", err, raw)
	}
	return parsed
}

// TestWriteHooksWrites32Keys is the plan-level success criterion: writeHooks
// into an empty dir writes 32 keys under hooks.
func TestWriteHooksWrites32Keys(t *testing.T) {
	parsed := writeAndParseHooks(t, "/opt/openbox/bin/openbox")
	if got, want := len(parsed.Hooks), 32; got != want {
		names := make([]string, 0, len(parsed.Hooks))
		for n := range parsed.Hooks {
			names = append(names, n)
		}
		t.Fatalf("wrote %d hook keys, want %d: %v", got, want, names)
	}
}

// TestAsyncRegistrationTable pins D5's sync/async split by event, exactly.
// ConfigChange writes no async key and timeout 30 (the one armed hook, phase
// 09); the 15 async rows write "async": true and timeout 5; the 5 other new
// sync rows (PermissionRequest, TaskCreated, TaskCompleted, Elicitation,
// ElicitationResult) write no async key at all -- "async": false must never
// appear, or absent and false become two states.
func TestAsyncRegistrationTable(t *testing.T) {
	parsed := writeAndParseHooks(t, "/opt/openbox/bin/openbox")

	asyncTrue := []string{
		"Setup", "InstructionsLoaded", "UserPromptExpansion", "MessageDisplay",
		"PostToolBatch", "Notification", "TeammateIdle", "CwdChanged",
		"DirectoryAdded", "FileChanged", "WorktreeRemove", "PreCompact",
		"PostCompact", "PreModelSwitch", "PostModelSwitch",
	}
	if len(asyncTrue) != 15 {
		t.Fatalf("test fixture asyncTrue has %d entries, want 15", len(asyncTrue))
	}
	for _, name := range asyncTrue {
		entries, ok := parsed.Hooks[name]
		if !ok || len(entries) == 0 || len(entries[0].Hooks) == 0 {
			t.Errorf("%s: no handler registered", name)
			continue
		}
		h := entries[0].Hooks[0]
		if h.Async == nil || !*h.Async {
			t.Errorf("%s: async = %v, want true", name, h.Async)
		}
		if h.Timeout != otherHookTimeoutSec {
			t.Errorf("%s: timeout = %d, want %d", name, h.Timeout, otherHookTimeoutSec)
		}
	}

	noAsyncKey := []string{"PermissionRequest", "TaskCreated", "TaskCompleted", "Elicitation", "ElicitationResult"}
	for _, name := range noAsyncKey {
		entries, ok := parsed.Hooks[name]
		if !ok || len(entries) == 0 || len(entries[0].Hooks) == 0 {
			t.Errorf("%s: no handler registered", name)
			continue
		}
		h := entries[0].Hooks[0]
		if h.Async != nil {
			t.Errorf("%s: async key present (%v), want absent", name, *h.Async)
		}
		if h.Timeout != otherHookTimeoutSec {
			t.Errorf("%s: timeout = %d, want %d", name, h.Timeout, otherHookTimeoutSec)
		}
	}

	entries, ok := parsed.Hooks["ConfigChange"]
	if !ok || len(entries) == 0 || len(entries[0].Hooks) == 0 {
		t.Fatal("ConfigChange: no handler registered")
	}
	cc := entries[0].Hooks[0]
	if cc.Async != nil {
		t.Errorf("ConfigChange: async key present (%v), want absent", *cc.Async)
	}
	if cc.Timeout != preToolUseHookTimeoutSec {
		t.Errorf("ConfigChange: timeout = %d, want %d (the one armed hook)", cc.Timeout, preToolUseHookTimeoutSec)
	}
}

// TestExistingRowsTimeoutUnchangedByOtherHookTimeoutSecConstant proves step 10
// is byte-identical: replacing the literal 5 with otherHookTimeoutSec must not
// move any existing row's written timeout.
func TestExistingRowsTimeoutUnchangedByOtherHookTimeoutSecConstant(t *testing.T) {
	parsed := writeAndParseHooks(t, "/opt/openbox/bin/openbox")
	want := map[string]int{
		"SessionStart":       5,
		"UserPromptSubmit":   preToolUseHookTimeoutSec,
		"PreToolUse":         preToolUseHookTimeoutSec,
		"PostToolUse":        5,
		"PostToolUseFailure": 5,
		"Stop":               5,
		"SubagentStop":       5,
		"SubagentStart":      5,
		"PermissionDenied":   5,
		"StopFailure":        5,
		"SessionEnd":         15,
	}
	for name, wantTimeout := range want {
		entries, ok := parsed.Hooks[name]
		if !ok || len(entries) == 0 || len(entries[0].Hooks) == 0 {
			t.Errorf("%s: no handler registered", name)
			continue
		}
		if got := entries[0].Hooks[0].Timeout; got != wantTimeout {
			t.Errorf("%s: timeout = %d, want %d", name, got, wantTimeout)
		}
	}
}

// TestReconcileLocalHook_UpgradeDropsStaleAsyncKey is the second-invocation
// case the create branch alone cannot exercise: a settings file written by an
// older install (or hand-edited) carries "async": true on a row this version
// says must be sync (ConfigChange, Async:false). reconcileLocalHook must be
// taught to delete the key on the *second* writeHooks call against that
// existing entry, exactly as it already deletes statusMessage when empty --
// otherwise a re-run of `openbox init` against an existing install leaves the
// flag stale (risk table row 4) and ConfigChange's gating decision becomes a
// no-op that reports success.
func TestReconcileLocalHook_UpgradeDropsStaleAsyncKey(t *testing.T) {
	const engine = "/opt/openbox/bin/openbox"
	project := t.TempDir()
	settingsPath := filepath.Join(project, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}

	command := localHookCommand(engine, "hook claude-code ConfigChange")
	stale := `{"hooks":{"ConfigChange":[{"matcher":"","hooks":[{"type":"command","command":` +
		mustJSON(t, command) + `,"timeout":30,"async":true,"statusMessage":"stale"}]}]}}`
	if err := os.WriteFile(settingsPath, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	// First invocation against the pre-existing file: this call is the one that
	// must go through reconcileLocalHook (hasLocalHookCommand is already true),
	// not the create branch -- the create branch is never exercised for this
	// row in this test, which is the point.
	if err := writeHooks(settingsPath, engine); err != nil {
		t.Fatalf("writeHooks: %v", err)
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed asyncFixture
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v\n%s", err, raw)
	}
	entries := parsed.Hooks["ConfigChange"]
	if len(entries) == 0 || len(entries[0].Hooks) == 0 {
		t.Fatalf("ConfigChange handler missing after reconcile:\n%s", raw)
	}
	h := entries[0].Hooks[0]
	if h.Async != nil {
		t.Fatalf("after reconcile (invocation against a pre-existing stale entry), async = %v, want key absent:\n%s",
			*h.Async, raw)
	}
	if h.Timeout != preToolUseHookTimeoutSec {
		t.Errorf("ConfigChange timeout = %d, want %d", h.Timeout, preToolUseHookTimeoutSec)
	}

	// Second invocation: re-running over the now-correct file must be a no-op
	// (idempotent), proving the delete-when-false path does not flap.
	second, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeHooks(settingsPath, engine); err != nil {
		t.Fatalf("second writeHooks: %v", err)
	}
	third, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(third) {
		t.Errorf("second writeHooks over its own reconciled output was not a no-op.\n--- before ---\n%s\n--- after ---\n%s",
			second, third)
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestFileChangedMatcherIsTheWatchList pins D7: the matcher IS the watch
// list, no wildcard.
func TestFileChangedMatcherIsTheWatchList(t *testing.T) {
	if fileChangedMatcher != ".env|.envrc|.mcp.json|CLAUDE.md" {
		t.Errorf("fileChangedMatcher = %q, want %q", fileChangedMatcher, ".env|.envrc|.mcp.json|CLAUDE.md")
	}
	parsed := writeAndParseHooks(t, "/opt/openbox/bin/openbox")
	entries := parsed.Hooks["FileChanged"]
	if len(entries) == 0 {
		t.Fatal("FileChanged: no handler registered")
	}
	if got := entries[0].Matcher; got != fileChangedMatcher {
		t.Errorf("FileChanged written matcher = %q, want %q", got, fileChangedMatcher)
	}
}
