package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocalHookEventNamesCarryNoPathSyntax. Each event name goes straight into
// a gjson/sjson path, and gjson reads `.` as a separator and `*?#|@` as query
// syntax. An escaper would be code no input exercises; this holds the property
// instead, and fails the day an event name needs one.
func TestLocalHookEventNamesCarryNoPathSyntax(t *testing.T) {
	const meta = `.*?#|@\`
	for _, ev := range localHookEvents {
		if ev.Event == "" {
			t.Error("an empty event name would address the hooks object itself")
		}
		if strings.ContainsAny(ev.Event, meta) {
			t.Errorf("event name %q contains gjson path syntax; localHookPath would address the wrong "+
				"node, so this needs an escaper before the name can ship", ev.Event)
		}
	}
}

// TestWriteLocalHooksKeepsTheDevelopersOtherSettings. This file lives inside
// the developer's repository and holds their permissions, their model choice
// and whatever else they put there. Round-tripping it through map[string]any
// preserved every key and alphabetised all of them, plus reindented the whole
// document -- a diff in their working tree that OpenBox was not asked to make.
func TestWriteLocalHooksKeepsTheDevelopersOtherSettings(t *testing.T) {
	project := t.TempDir()
	settingsPath := filepath.Join(project, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	const existing = `{
  "zzz_written_last": true,
  "permissions": {
    "allow": [
        "Bash(git*)"
    ]
  },
  "hooks": {
    "WorktreeCreate": [
      {"matcher": "*", "hooks": [{"type": "command", "command": "notify-send hi"}]}
    ]
  },
  "aaa_written_first": 1
}
`
	if err := os.WriteFile(settingsPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeHooks(ProjectSettingsPath(project), "/usr/local/bin/openbox"); err != nil {
		t.Fatalf("writeHooks: %v", err)
	}

	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)

	if i, j := strings.Index(got, "zzz_written_last"), strings.Index(got, "aaa_written_first"); i < 0 || j < 0 || i > j {
		t.Errorf("the developer's top-level keys were dropped or alphabetised:\n%s", got)
	}
	if !strings.Contains(got, `"Bash(git*)"`) {
		t.Errorf("a permissions entry was lost:\n%s", got)
	}
	if !strings.Contains(got, "notify-send hi") {
		t.Errorf("a foreign hook event was lost:\n%s", got)
	}
	// WorktreeCreate, not Notification: a real Claude Code event OpenBox
	// PERMANENTLY refuses to register (hookevent.go:40) is the only fixture
	// that keeps testing "we preserve what is not ours" honestly. Notification
	// joined the 32-event vocabulary in v1.8, so writeHooks now APPENDS
	// its own handler alongside a foreign Notification entry instead of
	// leaving it untouched -- a key that can become ours cannot prove this.
	if !strings.Contains(got, "Bash(git*)\"\n") && !strings.Contains(got, "        \"Bash(git*)\"") {
		t.Errorf("the developer's four-space indentation inside permissions was reformatted:\n%s", got)
	}

	// And the hooks it owns really are there, or the assertions above are vacuous.
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the written file does not parse: %v\n%s", err, got)
	}
	if _, ok := doc.Hooks["WorktreeCreate"]; !ok {
		t.Errorf("the foreign WorktreeCreate event is gone from hooks:\n%s", got)
	}
	for _, ev := range localHookEvents {
		var found bool
		for _, group := range doc.Hooks[ev.Event] {
			for _, h := range group.Hooks {
				if strings.Contains(h.Command, "hook claude-code "+ev.Event) {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("no OpenBox handler for %s; the preservation assertions prove nothing:\n%s", ev.Event, got)
		}
	}
}

// TestWriteThinkingSummariesKeepsTheDevelopersOtherSettings is
// TestWriteLocalHooksKeepsTheDevelopersOtherSettings' sibling for the other
// writer of this same file: writeThinkingSummaries only ever touches
// showThinkingSummaries, so everything else -- key order, indentation,
// permissions, a developer's own hooks -- must survive exactly as writeHooks
// itself already has to preserve it.
func TestWriteThinkingSummariesKeepsTheDevelopersOtherSettings(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir() // deliberately not project: the record must not assume the two coincide
	settingsPath := filepath.Join(project, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	const existing = `{
  "zzz_written_last": true,
  "permissions": {
    "allow": [
        "Bash(git*)"
    ]
  },
  "hooks": {
    "WorktreeCreate": [
      {"matcher": "*", "hooks": [{"type": "command", "command": "notify-send hi"}]}
    ]
  },
  "aaa_written_first": 1
}
`
	if err := os.WriteFile(settingsPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeThinkingSummaries(settingsPath, home); err != nil {
		t.Fatalf("writeThinkingSummaries: %v", err)
	}

	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)

	if i, j := strings.Index(got, "zzz_written_last"), strings.Index(got, "aaa_written_first"); i < 0 || j < 0 || i > j {
		t.Errorf("the developer's top-level keys were dropped or alphabetised:\n%s", got)
	}
	if !strings.Contains(got, `"Bash(git*)"`) {
		t.Errorf("a permissions entry was lost:\n%s", got)
	}
	if !strings.Contains(got, "notify-send hi") {
		t.Errorf("a foreign hook event was lost:\n%s", got)
	}
	if !strings.Contains(got, "Bash(git*)\"\n") && !strings.Contains(got, "        \"Bash(git*)\"") {
		t.Errorf("the developer's four-space indentation inside permissions was reformatted:\n%s", got)
	}
	if !strings.Contains(got, "showThinkingSummaries") {
		t.Errorf("showThinkingSummaries was not written at all:\n%s", got)
	}

	var doc struct {
		ShowThinkingSummaries bool           `json:"showThinkingSummaries"`
		Hooks                 map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the written file does not parse: %v\n%s", err, got)
	}
	if !doc.ShowThinkingSummaries {
		t.Errorf("showThinkingSummaries did not parse as true:\n%s", got)
	}
	if _, ok := doc.Hooks["WorktreeCreate"]; !ok {
		t.Errorf("the foreign WorktreeCreate hook event is gone:\n%s", got)
	}
}

// TestWriteLocalHooksIsIdempotentByteForByte. `openbox init` is re-run
// routinely, and a second run that rewrites the file gives the developer a
// spurious diff every time.
func TestWriteLocalHooksIsIdempotentByteForByte(t *testing.T) {
	project := t.TempDir()
	settingsPath := filepath.Join(project, ".claude", "settings.local.json")

	if err := writeHooks(ProjectSettingsPath(project), "/usr/local/bin/openbox"); err != nil {
		t.Fatalf("first writeHooks: %v", err)
	}
	first, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeHooks(ProjectSettingsPath(project), "/usr/local/bin/openbox"); err != nil {
		t.Fatalf("second writeHooks: %v", err)
	}
	second, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("a second install rewrote the file.\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// TestWriteLocalHooksRefusesANonArrayEvent. The old code read a non-array event
// as absent and then overwrote it. A shape somebody chose is not ours to
// replace, and the same posture already applies to the file as a whole.
func TestWriteLocalHooksRefusesANonArrayEvent(t *testing.T) {
	project := t.TempDir()
	settingsPath := filepath.Join(project, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	const wrongShape = `{"hooks": {"PreToolUse": "not an array"}}`
	if err := os.WriteFile(settingsPath, []byte(wrongShape), 0o644); err != nil {
		t.Fatal(err)
	}

	err := writeHooks(ProjectSettingsPath(project), "/usr/local/bin/openbox")
	if err == nil {
		t.Fatal("writeHooks replaced a non-array hooks event")
	}
	if !strings.Contains(err.Error(), "not a JSON array") {
		t.Errorf("error does not name the shape problem: %v", err)
	}
	raw, _ := os.ReadFile(settingsPath)
	if string(raw) != wrongShape {
		t.Errorf("the refusal still rewrote the file:\n%s", raw)
	}
}
