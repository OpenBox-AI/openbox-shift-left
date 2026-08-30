package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
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
		DID: testDID,
	}
	if err := inst.Install(ref); err != nil {
		t.Fatalf("install: %v", err)
	}

	for _, rel := range []string{".claude-plugin/plugin.json", "hooks/hooks.json"} {
		if _, err := os.Stat(filepath.Join(pluginDir, rel)); err != nil {
			t.Errorf("missing bundle file %s: %v", rel, err)
		}
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg DevConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.DID != testDID {
		t.Errorf("config coordinates wrong: %+v", cfg)
	}
	if strings.Contains(string(raw), "obx_") {
		t.Errorf("config leaked a credential value: %s", raw)
	}

	if err := inst.Install(ref); err != nil {
		t.Fatalf("re-install: %v", err)
	}
}

// TestInstaller_PersistsEnforcePosture proves that decision onboarding change:
// the enforce posture chosen at `init` time (ref.Enforce/Tier2/Findings, set
// by --enforce) is written to dev.json, so the runtime hook reads it with NO
// env var.
func TestInstaller_PersistsEnforcePosture(t *testing.T) {
	pluginDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "openbox", "dev.json")
	inst := Installer{PluginDir: pluginDir, ConfigPath: cfgPath}

	tru := true
	ref := CredentialRef{
		DID:      testDID,
		Enforce:  &tru,
		Tier2:    &tru,
		Findings: &tru,
	}
	if err := inst.Install(ref); err != nil {
		t.Fatalf("install: %v", err)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg DevConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.Enforce == nil || !*cfg.Enforce {
		t.Error("enforce not persisted to dev.json")
	}
	if cfg.Tier2 == nil || !*cfg.Tier2 {
		t.Errorf("tier2 not persisted: %+v", cfg.Tier2)
	}
	if cfg.Findings == nil || !*cfg.Findings {
		t.Errorf("findings not persisted: %+v", cfg.Findings)
	}

	// Point the config loader at the file just written and ensure the env
	// overrides are truly absent (LookupEnv must report !ok, so config wins).
	t.Setenv(envConfigPath, cfgPath)
	for _, k := range []string{envEnforce, envTier2, envFindings} {
		if _, ok := os.LookupEnv(k); ok {
			orig := os.Getenv(k)
			os.Unsetenv(k)
			t.Cleanup(func() { os.Setenv(k, orig) })
		}
	}
	if !ResolveEnforce() {
		t.Error("ResolveEnforce() = false; expected the persisted enforce posture to win with no env override")
	}
	if !ResolveTier2() {
		t.Error("ResolveTier2() = false; expected persisted tier2")
	}
	if !ResolveFindings() {
		t.Error("ResolveFindings() = false; expected persisted findings")
	}
}

func TestInstaller_Plan(t *testing.T) {
	inst := Installer{PluginDir: "/x/plugins", ConfigPath: "/x/dev.json"}
	plan := inst.Plan(CredentialRef{DID: testDID})
	for _, want := range []string{"enabledPlugins", "openbox-observe", testDID, "metadata-only"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan missing %q:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "obx_") {
		t.Errorf("plan leaked a credential: %s", plan)
	}
}

// TestInstaller_ReInstallIsByteIdentical re-init must be byte-identical: a
// second Install with the same ref overwrites the bundle + config with the
// same content (idempotency, not just no-error).
func TestInstaller_ReInstallIsByteIdentical(t *testing.T) {
	pluginDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	inst := Installer{PluginDir: pluginDir, ConfigPath: cfgPath}
	ref := CredentialRef{DID: testDID}

	if err := inst.Install(ref); err != nil {
		t.Fatalf("first install: %v", err)
	}
	cfg1, _ := os.ReadFile(cfgPath)
	manifest1, _ := os.ReadFile(filepath.Join(pluginDir, ".claude-plugin", "plugin.json"))

	if err := inst.Install(ref); err != nil {
		t.Fatalf("re-install: %v", err)
	}
	cfg2, _ := os.ReadFile(cfgPath)
	manifest2, _ := os.ReadFile(filepath.Join(pluginDir, ".claude-plugin", "plugin.json"))

	if string(cfg1) != string(cfg2) {
		t.Errorf("dev config not byte-identical across re-init:\n%s\n---\n%s", cfg1, cfg2)
	}
	if string(manifest1) != string(manifest2) {
		t.Errorf("plugin manifest not byte-identical across re-init")
	}
}

// TestInstaller_PlacesEngineBinary story-SL4-wire-2: when EngineBinary is set,
// Install copies the unified engine into the bundle's bin/openbox
// (executable), idempotently.
func TestInstaller_PlacesEngineBinary(t *testing.T) {
	pluginDir := t.TempDir()
	engine := filepath.Join(t.TempDir(), "openbox")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	inst := Installer{
		PluginDir:    pluginDir,
		ConfigPath:   filepath.Join(t.TempDir(), "dev.json"),
		EngineBinary: engine,
	}
	if err := inst.Install(CredentialRef{DID: testDID}); err != nil {
		t.Fatalf("install: %v", err)
	}
	placed := filepath.Join(pluginDir, "bin", "openbox")
	fi, err := os.Stat(placed)
	if err != nil {
		t.Fatalf("engine not placed at bin/openbox: %v", err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("placed engine is not executable: %v", fi.Mode())
	}
	if got, _ := os.ReadFile(placed); !strings.Contains(string(got), "exit 0") {
		t.Errorf("placed engine content mismatch: %q", got)
	}
	if err := inst.Install(CredentialRef{DID: testDID}); err != nil {
		t.Fatalf("re-install: %v", err)
	}
}

func TestInstaller_SkipsEngineBinaryWhenUnset(t *testing.T) {
	pluginDir := t.TempDir()
	inst := Installer{PluginDir: pluginDir, ConfigPath: filepath.Join(t.TempDir(), "dev.json")}
	if err := inst.Install(CredentialRef{DID: testDID}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pluginDir, "bin", "openbox")); !os.IsNotExist(err) {
		t.Errorf("bin/openbox should not exist when EngineBinary is unset (err=%v)", err)
	}
}

func TestInstaller_RequiresDID(t *testing.T) {
	inst := Installer{PluginDir: t.TempDir(), ConfigPath: filepath.Join(t.TempDir(), "dev.json")}
	if err := inst.Install(CredentialRef{}); err == nil {
		t.Error("install without a DID should error")
	}
}

// TestInstaller_ReInitKeepsEnforcePosture a re-init that says nothing about
// posture must leave it alone.
func TestInstaller_ReInitKeepsEnforcePosture(t *testing.T) {
	pluginDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "openbox", "dev.json")
	inst := Installer{PluginDir: pluginDir, ConfigPath: cfgPath}
	tru := true

	if err := inst.Install(CredentialRef{
		DID:     testDID,
		AgentID: "agent-1", BackendURL: "https://backend.example",
		Enforce: &tru, Tier2: &tru, Findings: &tru,
	}); err != nil {
		t.Fatalf("install: %v", err)
	}

	if err := inst.Install(CredentialRef{
		DID: testDID,
	}); err != nil {
		t.Fatalf("re-install: %v", err)
	}

	cfg, err := devconfig.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enforce == nil || !*cfg.Enforce || cfg.Tier2 == nil || !*cfg.Tier2 || cfg.Findings == nil || !*cfg.Findings {
		t.Errorf("re-init downgraded the enforce posture: %+v", cfg)
	}
	if cfg.AgentID != "agent-1" || cfg.BackendURL != "https://backend.example" {
		t.Errorf("re-init dropped the sync coordinates: %+v", cfg)
	}
}
