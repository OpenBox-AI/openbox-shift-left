package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstaller_MaterializesBundleAndConfig(t *testing.T) {
	pluginDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "openbox", "dev.json")
	inst := Installer{PluginDir: pluginDir, ConfigPath: cfgPath}

	if inst.Name() != "claude-code" {
		t.Errorf("Name = %q", inst.Name())
	}
	if !inst.Available() {
		t.Error("adapter must report Available()==true (not the SL-2 stub)")
	}

	ref := CredentialRef{
		SecretService:     "openbox-dev",
		APIKeyAccount:     "apikey",
		PrivateKeyAccount: "seed",
		DID:               testDID,
		ContentCapture:    false,
	}
	if err := inst.Install(ref); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Bundle materialized, including the dotted .claude-plugin dir (go:embed all:).
	for _, rel := range []string{".claude-plugin/plugin.json", "hooks/hooks.json"} {
		if _, err := os.Stat(filepath.Join(pluginDir, rel)); err != nil {
			t.Errorf("missing bundle file %s: %v", rel, err)
		}
	}

	// Config written with coordinates and NO secret values (INV-1).
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg DevConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.DID != testDID || cfg.SecretService != "openbox-dev" || cfg.APIKeyAccount != "apikey" {
		t.Errorf("config coordinates wrong: %+v", cfg)
	}
	// The written file must never contain an obx_ key or a seed value.
	if strings.Contains(string(raw), "obx_") {
		t.Errorf("config leaked a credential value: %s", raw)
	}

	// Idempotent: a second install with the same ref succeeds.
	if err := inst.Install(ref); err != nil {
		t.Fatalf("re-install: %v", err)
	}
}

func TestInstaller_Plan(t *testing.T) {
	inst := Installer{PluginDir: "/x/plugins", ConfigPath: "/x/dev.json"}
	plan := inst.Plan(CredentialRef{DID: testDID, SecretService: "svc", APIKeyAccount: "k", PrivateKeyAccount: "s"})
	for _, want := range []string{"enabledPlugins", "openbox-observe", testDID, "metadata-only"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan missing %q:\n%s", want, plan)
		}
	}
	// Plan must not print a secret (it never receives one, but guard anyway).
	if strings.Contains(plan, "obx_") {
		t.Errorf("plan leaked a credential: %s", plan)
	}
}

func TestInstaller_RequiresDID(t *testing.T) {
	inst := Installer{PluginDir: t.TempDir(), ConfigPath: filepath.Join(t.TempDir(), "dev.json")}
	if err := inst.Install(CredentialRef{}); err == nil {
		t.Error("install without a DID should error")
	}
}
