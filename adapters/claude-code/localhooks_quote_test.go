package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A home directory with a space is ordinary — an account named for a person on
// macOS or Windows. Unquoted, the shell splits the command and every hook in the
// project silently fails to start. Since ADR-0016 made project scope the DEFAULT,
// that would break governance for those users on a plain `openbox init`.
func TestLocalHookCommandQuotesTheEnginePath(t *testing.T) {
	dir := t.TempDir()
	engine := filepath.Join("/Users/John Doe/.claude/plugins/openbox-observe/bin", "openbox")
	if err := writeLocalHooks(dir, engine); err != nil {
		t.Fatalf("writeLocalHooks: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string } `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for event, entries := range settings.Hooks {
		for _, entry := range entries {
			for _, h := range entry.Hooks {
				seen++
				if !strings.HasPrefix(h.Command, `"`+engine+`"`) {
					t.Errorf("%s command does not quote the engine path: %q", event, h.Command)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no hook commands written")
	}
}

// The merge must stay idempotent ACROSS the quoting change: an install written by
// an earlier version carries unquoted commands, and appending a quoted duplicate
// beside it would make every hook fire twice.
func TestLocalHooksIdempotentAgainstAnUnquotedLegacyEntry(t *testing.T) {
	dir := t.TempDir()
	engine := "/opt/openbox/bin/openbox"
	settingsPath := filepath.Join(dir, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Exactly what a pre-quoting install left behind.
	legacy := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"` + engine + ` hook claude-code SessionStart","timeout":5}]}]}}`
	if err := os.WriteFile(settingsPath, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeLocalHooks(dir, engine); err != nil {
		t.Fatalf("writeLocalHooks: %v", err)
	}
	raw, _ := os.ReadFile(settingsPath)
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string } `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, entry := range settings.Hooks["SessionStart"] {
		n += len(entry.Hooks)
	}
	if n != 1 {
		t.Errorf("SessionStart has %d handlers, want 1 — a re-init duplicated the hook across the quoting change", n)
	}
}
