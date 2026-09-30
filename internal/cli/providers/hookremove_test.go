package providers_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

func seed(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRemoveProviderHooksReachesBothAdapters. cmd/openbox cannot import an
// adapter, so this registry is the only door to either remover — and an
// uninstall that silently reached neither would report a clean machine while
// every hook kept firing.
func TestRemoveProviderHooksReachesBothAdapters(t *testing.T) {
	for _, tc := range []struct {
		name, file, body, gone string
	}{
		{
			name: "claude-code",
			file: "settings.json",
			body: `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"\"/o/openbox\" hook claude-code SessionStart"}]}]}}`,
			gone: "hook claude-code",
		},
		{
			name: "codex",
			file: "hooks.json",
			body: `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"\"/o/openbox\" hook codex SessionStart"}]}]}}`,
			gone: "hook codex",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := seed(t, tc.file, tc.body)
			removed, err := providers.RemoveProviderHooks(tc.name, path)
			if err != nil {
				t.Fatalf("RemoveProviderHooks: %v", err)
			}
			if len(removed) != 1 {
				t.Fatalf("removed = %v; want one entry", removed)
			}
			if !strings.Contains(removed[0], "SessionStart") {
				t.Errorf("the report %q does not name the event", removed[0])
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), tc.gone) {
				t.Errorf("%q survived removal:\n%s", tc.gone, raw)
			}
		})
	}
}

// TestRemoveProviderHooksRefusesAnUnknownProvider. A typo must not read as
// "nothing was installed for it".
func TestRemoveProviderHooksRefusesAnUnknownProvider(t *testing.T) {
	_, err := providers.RemoveProviderHooks("clod-code", seed(t, "settings.json", "{}"))
	if !errors.Is(err, provider.ErrUnknown) {
		t.Fatalf("RemoveProviderHooks on an unknown name = %v; want provider.ErrUnknown", err)
	}
}

// TestRemoveProviderHooksForAProviderWithNoHookFile. A machine that only ever
// ran one tool is the common case, and uninstall walks every surface anyway.
func TestRemoveProviderHooksForAProviderWithNoHookFile(t *testing.T) {
	for _, name := range []string{"claude-code", "codex"} {
		path := filepath.Join(t.TempDir(), "absent", "hooks.json")
		removed, err := providers.RemoveProviderHooks(name, path)
		if err != nil || removed != nil {
			t.Errorf("%s: RemoveProviderHooks on an absent file = %v, %v; want nil, nil", name, removed, err)
		}
	}
}

// TestOwnedSpoolDirsAreDeDuplicated is the reason this lives here rather than
// being hardcoded a fourth time in cmd/openbox. Each adapter has its own
// subdirectory under <user-config>/openbox, but OPENBOX_SPOOL_DIR overrides the
// whole path — so with it set both adapters resolve to the same directory, and
// an uninstall that took the list at face value would report deleting it twice.
func TestOwnedSpoolDirsAreDeDuplicated(t *testing.T) {
	shared := t.TempDir()
	t.Setenv(devconfig.EnvSpoolDir, shared)
	dirs := providers.OwnedSpoolDirs()
	if len(dirs) != 1 || dirs[0] != shared {
		t.Fatalf("OwnedSpoolDirs() = %v; want exactly [%s] under the override", dirs, shared)
	}
}

// TestOwnedSpoolDirsCoversEveryAdapter. A renamed or added spool that escaped
// this list would survive a full purge and keep undelivered governed tool calls
// on a machine the operator was told is clean.
func TestOwnedSpoolDirsCoversEveryAdapter(t *testing.T) {
	t.Setenv(devconfig.EnvSpoolDir, "")
	dirs := providers.OwnedSpoolDirs()
	if len(dirs) != len(provider.Supported()) {
		t.Fatalf("OwnedSpoolDirs() = %v; want one per adapter", dirs)
	}
	seen := map[string]bool{}
	for _, d := range dirs {
		if !filepath.IsAbs(d) {
			t.Errorf("%q is not absolute; a RemoveAll target must never resolve against the cwd", d)
		}
		if seen[d] {
			t.Errorf("%q is listed twice", d)
		}
		seen[d] = true
	}
	for _, want := range []string{"cc-spool", "codex-spool"} {
		found := false
		for _, d := range dirs {
			if filepath.Base(d) == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no owned spool dir ends in %q: %v", want, dirs)
		}
	}
}

// TestMuseHasAnArmAtEveryRegistrySwitch a recognized name that fell through to
// a default arm would read as "unknown" at one entry point and "supported" at
// another.
func TestMuseHasAnArmAtEveryRegistrySwitch(t *testing.T) {
	name := string(provider.Muse)
	if _, err := providers.Engine(name); err != nil {
		t.Errorf("Engine(muse): %v", err)
	}
	if _, err := providers.Lookup(name); err != nil {
		t.Errorf("Lookup(muse): %v", err)
	}
	if providers.SpoolDirFor(name) == "" {
		t.Error("SpoolDirFor(muse) is empty")
	}
	if len(providers.HookMarkers(name)) == 0 {
		t.Error("HookMarkers(muse) is empty")
	}
	if _, err := providers.RestoreProviderSettings(name, filepath.Join(t.TempDir(), "s.json"), t.TempDir()); err != nil {
		t.Errorf("RestoreProviderSettings(muse): %v", err)
	}
}

// TestMuseRemoveLeavesAnUnownedSettingsFileAlone a Muse settings file with no
// OpenBox handler in it is the developer's, byte for byte.
func TestMuseRemoveLeavesAnUnownedSettingsFileAlone(t *testing.T) {
	body := `{"schema_version":1,"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"my-audit"}]}]}}`
	path := seed(t, "settings.json", body)
	removed, err := providers.RemoveProviderHooks(string(provider.Muse), path)
	if err != nil || len(removed) != 0 {
		t.Fatalf("RemoveProviderHooks = %v, %v; want nothing removed", removed, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != body {
		t.Fatalf("settings changed:\n%s", got)
	}
}

// TestMuseInstallerRefusesUntilBuilt `init --provider muse` must fail loudly
// rather than half-install.
func TestMuseInstallerRefusesUntilBuilt(t *testing.T) {
	inst, err := providers.Lookup(string(provider.Muse))
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Install(provider.CredentialRef{}); err == nil {
		t.Fatal("Muse installer accepted an install")
	}
}
