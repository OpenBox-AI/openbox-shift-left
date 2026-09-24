package claudecode

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

func TestInstaller_MaterializesBundleAndConfig(t *testing.T) {
	pluginDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "openbox", "dev.json")
	inst := Installer{
		PluginDir:  pluginDir,
		ConfigPath: cfgPath,
		// Pinned so the install cannot reach the real home's settings file.
		SettingsPath: filepath.Join(t.TempDir(), ".claude", "settings.json"),
		// Pinned so the prior-settings record cannot reach the real home either.
		HomeDir: t.TempDir(),
	}

	if inst.Name() != "claude-code" {
		t.Errorf("Name = %q", inst.Name())
	}
	if !inst.Available() {
		t.Error("adapter must report Available()==true (not the SL-2 stub)")
	}

	ref := CredentialRef{
		AgentID: testAgentID,
	}
	if err := inst.Install(ref); err != nil {
		t.Fatalf("install: %v", err)
	}

	// The bundle's one job is hosting the engine the registrations point at. It
	// must carry no plugin manifest and no second copy of the hook config: a
	// plugin's handlers do not de-duplicate against the settings-level ones, so
	// anything that loaded this directory would double every event.
	if _, err := os.Stat(filepath.Join(pluginDir, "bin")); err != nil {
		t.Errorf("the bundle has no bin/ for the engine: %v", err)
	}
	var manifests []string
	_ = filepath.WalkDir(pluginDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(path) == ".json" {
			manifests = append(manifests, path)
		}
		return nil
	})
	if len(manifests) > 0 {
		t.Errorf("the bundle still ships loadable manifest(s), which would double every event: %v", manifests)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg DevConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.AgentID != testAgentID {
		t.Errorf("config coordinates wrong: %+v", cfg)
	}
	if strings.Contains(string(raw), "obx_") {
		t.Errorf("config leaked a credential value: %s", raw)
	}

	if err := inst.Install(ref); err != nil {
		t.Fatalf("re-install: %v", err)
	}
}

// TestInstaller_PersistsPosture proves that decision onboarding change: the
// posture chosen at `init` time (ref.Tier2/Findings, set by the resolved
// posture) is written to dev.json, so the runtime hook reads it with NO env
// var. Enforce is no longer part of this: it has no CLI/install-time knob at
// all now (ResolveEnforce always reports true).
func TestInstaller_PersistsPosture(t *testing.T) {
	pluginDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "openbox", "dev.json")
	inst := Installer{
		PluginDir:  pluginDir,
		ConfigPath: cfgPath,
		// Pinned so the install cannot reach the real home's settings file.
		SettingsPath: filepath.Join(t.TempDir(), ".claude", "settings.json"),
		// Pinned so the prior-settings record cannot reach the real home either.
		HomeDir: t.TempDir(),
	}

	tru := true
	ref := CredentialRef{
		AgentID:  testAgentID,
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
	if cfg.Tier2 == nil || !*cfg.Tier2 {
		t.Errorf("tier2 not persisted: %+v", cfg.Tier2)
	}
	if cfg.Findings == nil || !*cfg.Findings {
		t.Errorf("findings not persisted: %+v", cfg.Findings)
	}

	// Point the config loader at the file just written and ensure the env
	// overrides are truly absent (LookupEnv must report !ok, so config wins).
	t.Setenv(envConfigPath, cfgPath)
	for _, k := range []string{envTier2, envFindings} {
		if _, ok := os.LookupEnv(k); ok {
			orig := os.Getenv(k)
			os.Unsetenv(k)
			t.Cleanup(func() { os.Setenv(k, orig) })
		}
	}
	if !ResolveTier2() {
		t.Error("ResolveTier2() = false; expected persisted tier2")
	}
	if !ResolveFindings() {
		t.Error("ResolveFindings() = false; expected persisted findings")
	}
}

// TestInstaller_ReInstallIsByteIdentical re-init must be byte-identical: a
// second Install with the same ref overwrites the bundle + config with the
// same content (idempotency, not just no-error).
func TestInstaller_ReInstallIsByteIdentical(t *testing.T) {
	pluginDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	inst := Installer{
		PluginDir:  pluginDir,
		ConfigPath: cfgPath,
		// Pinned so the install cannot reach the real home's settings file.
		SettingsPath: filepath.Join(t.TempDir(), ".claude", "settings.json"),
		// Pinned so the prior-settings record cannot reach the real home either.
		HomeDir: t.TempDir(),
	}
	ref := CredentialRef{AgentID: testAgentID}

	if err := inst.Install(ref); err != nil {
		t.Fatalf("first install: %v", err)
	}
	cfg1, _ := os.ReadFile(cfgPath)
	hooks1, err := os.ReadFile(inst.SettingsPath)
	if err != nil {
		t.Fatalf("first install wrote no hooks: %v", err)
	}

	if err := inst.Install(ref); err != nil {
		t.Fatalf("re-install: %v", err)
	}
	cfg2, _ := os.ReadFile(cfgPath)
	hooks2, _ := os.ReadFile(inst.SettingsPath)

	if string(cfg1) != string(cfg2) {
		t.Errorf("dev config not byte-identical across re-init:\n%s\n---\n%s", cfg1, cfg2)
	}
	// The bundle no longer ships a manifest to compare, and comparing two
	// absent files would have passed whatever the installer did. The hook
	// registrations are the artifact that has to be stable now -- and the file
	// they live in belongs to the developer.
	if string(hooks1) != string(hooks2) {
		t.Errorf("hook registrations not byte-identical across re-init:\n%s\n---\n%s", hooks1, hooks2)
	}
}

// TestInstaller_SetsThinkingSummariesAndRecordsThePriorValue is the
// installer-level wiring check for writeThinkingSummaries: Install must call
// it inside the same lock as writeHooks, against the same settings file and
// the same (test-pinned) home, and a fresh machine's prior value is "absent".
func TestInstaller_SetsThinkingSummariesAndRecordsThePriorValue(t *testing.T) {
	home := t.TempDir()
	inst := Installer{
		PluginDir:    t.TempDir(),
		ConfigPath:   filepath.Join(t.TempDir(), "dev.json"),
		SettingsPath: filepath.Join(t.TempDir(), ".claude", "settings.json"),
		HomeDir:      home,
	}
	if err := inst.Install(CredentialRef{AgentID: testAgentID}); err != nil {
		t.Fatalf("install: %v", err)
	}

	raw, err := os.ReadFile(inst.SettingsPath)
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

	recPath := PriorSettingsPath(home)
	recRaw, err := os.ReadFile(recPath)
	if err != nil {
		t.Fatalf("Install did not create the prior-settings record at %s: %v", recPath, err)
	}
	var rec priorSettings
	if err := json.Unmarshal(recRaw, &rec); err != nil {
		t.Fatalf("parse record: %v\n%s", err, recRaw)
	}
	pv, ok := rec.Keys[ThinkingSummariesKey]
	if !ok {
		t.Fatalf("record holds no entry for %s: %+v", ThinkingSummariesKey, rec)
	}
	if pv.Present {
		t.Errorf("recorded prior value: %+v, want Present=false (a fresh settings file has no such key)", pv)
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
		SettingsPath: filepath.Join(t.TempDir(), ".claude", "settings.json"),
		PluginDir:    pluginDir,
		ConfigPath:   filepath.Join(t.TempDir(), "dev.json"),
		EngineBinary: engine,
		HomeDir:      t.TempDir(),
	}
	if err := inst.Install(CredentialRef{AgentID: testAgentID}); err != nil {
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
	if err := inst.Install(CredentialRef{AgentID: testAgentID}); err != nil {
		t.Fatalf("re-install: %v", err)
	}
}

func TestInstaller_SkipsEngineBinaryWhenUnset(t *testing.T) {
	pluginDir := t.TempDir()
	inst := Installer{
		SettingsPath: filepath.Join(t.TempDir(), ".claude", "settings.json"), PluginDir: pluginDir, ConfigPath: filepath.Join(t.TempDir(), "dev.json"),
		HomeDir: t.TempDir()}
	if err := inst.Install(CredentialRef{AgentID: testAgentID}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pluginDir, "bin", "openbox")); !os.IsNotExist(err) {
		t.Errorf("bin/openbox should not exist when EngineBinary is unset (err=%v)", err)
	}
}

func TestInstallRequiresAgentID(t *testing.T) {
	inst := Installer{
		SettingsPath: filepath.Join(t.TempDir(), ".claude", "settings.json"), PluginDir: t.TempDir(), ConfigPath: filepath.Join(t.TempDir(), "dev.json"),
		HomeDir: t.TempDir()}
	if err := inst.Install(CredentialRef{}); err == nil {
		t.Error("install without an agent id should error")
	}
}

// TestInstaller_ReInitKeepsPosture a re-init that says nothing about posture
// must leave it alone. Enforce has no Update/CredentialRef field any more (no
// install-time knob left at all, since ResolveEnforce always reports true).
func TestInstaller_ReInitKeepsPosture(t *testing.T) {
	pluginDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "openbox", "dev.json")
	inst := Installer{
		PluginDir:  pluginDir,
		ConfigPath: cfgPath,
		// Pinned so the install cannot reach the real home's settings file.
		SettingsPath: filepath.Join(t.TempDir(), ".claude", "settings.json"),
		// Pinned so the prior-settings record cannot reach the real home either.
		HomeDir: t.TempDir(),
	}
	tru := true

	if err := inst.Install(CredentialRef{
		AgentID: "agent-1", BackendURL: "https://backend.example",
		Tier2: &tru, Findings: &tru,
	}); err != nil {
		t.Fatalf("install: %v", err)
	}

	if err := inst.Install(CredentialRef{
		AgentID: "agent-1",
	}); err != nil {
		t.Fatalf("re-install: %v", err)
	}

	cfg, err := devconfig.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tier2 == nil || !*cfg.Tier2 || cfg.Findings == nil || !*cfg.Findings {
		t.Errorf("re-init downgraded posture: %+v", cfg)
	}
	if cfg.AgentID != "agent-1" || cfg.BackendURL != "https://backend.example" {
		t.Errorf("re-init dropped the sync coordinates: %+v", cfg)
	}
}
