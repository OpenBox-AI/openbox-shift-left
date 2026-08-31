package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHookedEventNamesCarryNoPathSyntax. The event names go straight into a
// gjson/sjson path, and gjson reads `.` as a separator and `*?#|@` as query
// syntax. An escaper here would be code no input ever exercises; this holds the
// property instead, and fails the day somebody adds an event name that needs
// one.
func TestHookedEventNamesCarryNoPathSyntax(t *testing.T) {
	const meta = `.*?#|@\`
	for _, ev := range hookedEvents {
		if strings.ContainsAny(string(ev), meta) {
			t.Errorf("event name %q contains gjson path syntax; hooksEventPath would address the "+
				"wrong node, so this needs an escaper before the name can ship", ev)
		}
		if ev == "" {
			t.Error("an empty event name would address the hooks object itself")
		}
	}
	if len(hookedEvents) != 5 {
		t.Errorf("hookedEvents has %d entries; the installer's Plan output names five", len(hookedEvents))
	}
}

// TestWriteHooksKeepsWhatCodexWrote. hooks.json is Codex's file. Binding it to
// a struct with two fields dropped every top-level key the struct did not name,
// and re-marshalling the hooks map put the events in alphabetical order rather
// than the order Codex wrote them. Both are silent modifications of data that
// is not ours.
func TestWriteHooksKeepsWhatCodexWrote(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	// Deliberately: a top-level key the installer has never heard of, events out
	// of alphabetical order, and a foreign hook inside one of the events.
	const existing = `{
  "zzz_codex_own_setting": {"retain": true},
  "description": "Codex wrote this description",
  "hooks": {
    "SessionEnd": [],
    "PreToolUse": [
      {
        "matcher": "*",
        "hooks": [
          {"type": "command", "command": "my-audit-log", "timeout": 3}
        ]
      }
    ]
  }
}
`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	i := Installer{HooksPath: path, ConfigPath: filepath.Join(dir, "dev.json"), EngineBinary: "/usr/bin/openbox"}
	if err := i.writeHooks(); err != nil {
		t.Fatalf("writeHooks: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)

	if !strings.Contains(got, "zzz_codex_own_setting") {
		t.Errorf("a top-level key the installer does not own was dropped:\n%s", got)
	}
	if !strings.Contains(got, "Codex wrote this description") {
		t.Errorf("the installer overwrote a description it did not write:\n%s", got)
	}
	if !strings.Contains(got, "my-audit-log") {
		t.Errorf("a foreign hook inside PreToolUse was lost:\n%s", got)
	}
	if iEnd, iPre := strings.Index(got, `"SessionEnd"`), strings.Index(got, `"PreToolUse"`); iEnd > iPre {
		t.Errorf("the installer alphabetised Codex's event order: SessionEnd moved after PreToolUse:\n%s", got)
	}
	if iZZZ, iDesc := strings.Index(got, "zzz_codex_own_setting"), strings.Index(got, `"description"`); iZZZ > iDesc {
		t.Errorf("the installer alphabetised Codex's top-level keys:\n%s", got)
	}

	// And it really did its own job, or every assertion above is vacuous.
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
	for _, ev := range hookedEvents {
		var found bool
		for _, group := range doc.Hooks[string(ev)] {
			for _, h := range group.Hooks {
				if strings.Contains(h.Command, "hook codex "+string(ev)) {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("no OpenBox handler for %s; the preservation assertions above prove nothing:\n%s", ev, got)
		}
	}
}

// TestWriteHooksRefusesAnUnparsableFile. sjson edits a malformed document
// without complaint, so the guard that used to be a side effect of Unmarshal
// failing has to be explicit -- and this file is one the installer must never
// clobber when it cannot understand it.
func TestWriteHooksRefusesAnUnparsableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	const malformed = `{"hooks": {"PreToolUse": [},}`
	if err := os.WriteFile(path, []byte(malformed), 0o600); err != nil {
		t.Fatal(err)
	}

	i := Installer{HooksPath: path, ConfigPath: filepath.Join(dir, "dev.json")}
	err := i.writeHooks()
	if err == nil {
		t.Fatal("writeHooks rewrote a hooks.json it could not parse")
	}
	if !strings.Contains(err.Error(), "unparsable") {
		t.Errorf("error does not say why it refused: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != malformed {
		t.Errorf("the refusal still rewrote the file:\n%s", raw)
	}
}

// TestWriteHooksIsIdempotentByteForByte. `openbox init` is re-run routinely and
// hooks.json is Codex's file; a second run that reformats it is a change to
// somebody else's data with nothing behind it.
func TestWriteHooksIsIdempotentByteForByte(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	i := Installer{HooksPath: path, ConfigPath: filepath.Join(dir, "dev.json"), EngineBinary: "/usr/bin/openbox"}

	if err := i.writeHooks(); err != nil {
		t.Fatalf("first writeHooks: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.writeHooks(); err != nil {
		t.Fatalf("second writeHooks: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("a second install rewrote hooks.json.\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}
