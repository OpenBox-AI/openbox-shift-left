package providers_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// priorValueFixture and priorSettingsFixture reconstruct the on-disk record
// contract from the outside (schema/settings_path/keys/present/raw), the same
// way this package's own JSON fixtures in hookremove_test.go stand in for a
// settings file without importing the adapter that writes one.
type priorValueFixture struct {
	Present bool   `json:"present"`
	Raw     string `json:"raw,omitempty"`
}

type priorSettingsFixture struct {
	Schema string                       `json:"schema"`
	Keys   map[string]priorValueFixture `json:"keys"`
}

func seedPriorSettings(t *testing.T, home string, present bool, raw string) {
	t.Helper()
	recPath := providers.ClaudePriorSettingsPath(home)
	if err := os.MkdirAll(filepath.Dir(recPath), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(priorSettingsFixture{
		Schema: "openbox.claude-code.prior-settings/v1",
		Keys:   map[string]priorValueFixture{providers.ClaudeThinkingSummariesKey: {Present: present, Raw: raw}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func settingsFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRestoreProviderSettingsRestoresTheRecordedValue exercises the
// !Present branch (the key was absent before `init`) through the registry
// entry point, not the adapter directly -- this is the door cmd/openbox
// actually uses (TestOnlyTheRegistryImportsAdapters).
func TestRestoreProviderSettingsRestoresTheRecordedValue(t *testing.T) {
	home := t.TempDir()
	settingsPath := settingsFile(t, `{"showThinkingSummaries": true, "other": 1}`)
	seedPriorSettings(t, home, false, "")

	res, err := providers.RestoreProviderSettings("claude-code", settingsPath, home)
	if err != nil {
		t.Fatalf("RestoreProviderSettings: %v", err)
	}
	if !res.Recorded || res.Present || res.Drifted {
		t.Fatalf("unexpected result: %+v", res)
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "showThinkingSummaries") {
		t.Errorf("the key should have been deleted (it was absent before init): %s", raw)
	}
	if !strings.Contains(string(raw), `"other": 1`) {
		t.Errorf("an unrelated key was lost: %s", raw)
	}
}

// TestRestoreProviderSettingsCodexIsANoOp: Codex has no equivalent forced key,
// so uninstall must not error or touch its settings file when walking this
// surface unconditionally for every provider.
func TestRestoreProviderSettingsCodexIsANoOp(t *testing.T) {
	home := t.TempDir()
	settingsPath := settingsFile(t, `{}`)

	res, err := providers.RestoreProviderSettings("codex", settingsPath, home)
	if err != nil {
		t.Fatalf("RestoreProviderSettings(codex): %v", err)
	}
	if res.Recorded {
		t.Errorf("codex has no restore record; got %+v", res)
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{}" {
		t.Errorf("codex's settings file was rewritten: %s", raw)
	}
}

// TestRestoreProviderSettingsRefusesAnUnknownProvider: a typo must not read
// as "nothing was installed for it", mirroring
// TestRemoveProviderHooksRefusesAnUnknownProvider.
func TestRestoreProviderSettingsRefusesAnUnknownProvider(t *testing.T) {
	_, err := providers.RestoreProviderSettings("clod-code", settingsFile(t, "{}"), t.TempDir())
	if !errors.Is(err, provider.ErrUnknown) {
		t.Fatalf("RestoreProviderSettings on an unknown name = %v; want provider.ErrUnknown", err)
	}
}

// TestRestoreProviderSettingsOnAMachineWithNoRecordIsANoOp is criterion 6
// through the registry: a machine `init` never ran on for this provider (or
// one an earlier `uninstall` already cleaned) must report nothing and fail
// nothing.
func TestRestoreProviderSettingsOnAMachineWithNoRecordIsANoOp(t *testing.T) {
	home := t.TempDir()
	settingsPath := settingsFile(t, `{"other": 1}`)

	res, err := providers.RestoreProviderSettings("claude-code", settingsPath, home)
	if err != nil {
		t.Fatalf("RestoreProviderSettings: %v", err)
	}
	if res.Recorded {
		t.Errorf("Recorded = true with no record file present: %+v", res)
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"other": 1}` {
		t.Errorf("a no-record restore rewrote the settings file: %s", raw)
	}
}

// TestRestoreProviderSettingsMuseRestoresTelemetry exercises the Muse arm
// through the registry door: init's write, then the restore to the exact
// prior bytes, deleting the key when there was none.
func TestRestoreProviderSettingsMuseRestoresTelemetry(t *testing.T) {
	for _, tc := range []struct {
		name, before string
		wantPresent  bool
	}{
		{"absent before", "{\n  \"schema_version\": 1\n}\n", false},
		{"present before", "{\n  \"schema_version\": 1,\n  \"telemetry\": {\"enabled\": false}\n}\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			settingsPath := filepath.Join(t.TempDir(), "settings.json")
			if err := os.WriteFile(settingsPath, []byte(tc.before), 0o600); err != nil {
				t.Fatal(err)
			}
			replaced, err := providers.WriteMuseTelemetry(settingsPath, home, "http://127.0.0.1:8789")
			if err != nil {
				t.Fatal(err)
			}
			if (replaced != "") != tc.wantPresent {
				t.Errorf("replaced = %q, want a displaced value reported exactly when one existed", replaced)
			}
			if !providers.HasOwnedMuseTelemetry(settingsPath, home) {
				t.Fatal("the written value is not reported as owned")
			}
			res, err := providers.RestoreProviderSettings("muse", settingsPath, home)
			if err != nil || !res.Recorded || res.Drifted || res.Present != tc.wantPresent {
				t.Fatalf("result = %+v, %v", res, err)
			}
			if got, _ := os.ReadFile(settingsPath); string(got) != tc.before {
				t.Errorf("settings after restore = %q, want %q", got, tc.before)
			}
			if _, err := os.Stat(providers.MusePriorSettingsPath(home)); !os.IsNotExist(err) {
				t.Error("the restore record outlived a completed restore")
			}
			// Nothing recorded any more: a second uninstall is a quiet no-op.
			if res, err := providers.RestoreProviderSettings("muse", settingsPath, home); err != nil || res.Recorded {
				t.Errorf("second restore = %+v, %v", res, err)
			}
		})
	}
}
