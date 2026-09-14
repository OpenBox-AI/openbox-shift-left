package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
)

// TestInitSetsThinkingSummariesAndSaysSo drives `openbox init` through its
// real production wiring (providers.Lookup -> claudecode.Installer{}, no
// SettingsPath/HomeDir override) rather than a directly-constructed
// Installer, so this is the one test proving the real default path -- the
// one every developer actually runs -- forces the key and prints the
// required line (Requirements: "`init` prints one line saying the setting
// was changed and that uninstall restores it").
func TestInitSetsThinkingSummariesAndSaysSo(t *testing.T) {
	isolateHome(t)
	seedCredentials(t)
	a, out, errb := testApp(nil)
	if code := a.runDevInit([]string{"--provider", "claude-code"}); code != exitOK {
		t.Fatalf("openbox init --provider claude-code = %d, want 0; stderr=%q", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "showThinkingSummaries") {
		t.Errorf("init did not report the showThinkingSummaries change:\n%s", s)
	}
	if !strings.Contains(s, "openbox uninstall") {
		t.Errorf("init did not say `openbox uninstall` restores the previous value:\n%s", s)
	}

	settingsPath := gatewayservice.SettingsPath(os.Getenv("HOME"))
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var doc struct {
		ShowThinkingSummaries *bool `json:"showThinkingSummaries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse settings: %v\n%s", err, raw)
	}
	if doc.ShowThinkingSummaries == nil || !*doc.ShowThinkingSummaries {
		t.Errorf("showThinkingSummaries = %v, want true", doc.ShowThinkingSummaries)
	}
}

// TestInitCodexWritesNoThinkingSummariesKeyOrRecord is acceptance criterion
// 8's install-time half: Codex has no equivalent key, so `init` for it must
// name nothing and leave no restore record for a later `uninstall` to find.
func TestInitCodexWritesNoThinkingSummariesKeyOrRecord(t *testing.T) {
	isolateHome(t)
	seedCredentials(t, "codex")
	a, out, errb := testApp(nil)
	if code := a.runDevInit([]string{"--provider", "codex"}); code != exitOK {
		t.Fatalf("openbox init --provider codex = %d, want 0; stderr=%q", code, errb.String())
	}
	if strings.Contains(out.String(), "showThinkingSummaries") {
		t.Errorf("codex init mentioned showThinkingSummaries:\n%s", out.String())
	}
	if _, err := os.Stat(providers.ClaudePriorSettingsPath(os.Getenv("HOME"))); !os.IsNotExist(err) {
		t.Errorf("codex init created a claude-code prior-settings record (err=%v)", err)
	}
}
