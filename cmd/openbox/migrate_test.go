package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// TestInitMigratesLegacyPostureBeforeWritingOverIt migration must happen
// before THE first write, and only a command-level test can show it.
func TestInitMigratesLegacyPostureBeforeWritingOverIt(t *testing.T) {
	home := isolateHome(t)
	t.Setenv(devconfig.EnvConfigPath, "")
	legacyHome := t.TempDir()
	pointOSConfigDirAt(t, legacyHome)

	legacyDir := filepath.Join(legacyHome, legacyConfigSubdir())
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"developer_did":"did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301",` +
		`"content_capture":false,"org_signing_key_id":"key-42","enforce":false}`
	if err := os.WriteFile(filepath.Join(legacyDir, "dev.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteEnvFile(filepath.Join(home, ".env"), map[string]string{
		devconfig.EnvAPIKeyDirect:    "obx_test_k",
		devconfig.EnvAgentPrivateKey: testSeedB64,
	}); err != nil {
		t.Fatal(err)
	}

	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code", "--scope", "global"}); code != exitOK {
		t.Fatalf("exit = %d; stderr=%q", code, errb.String())
	}

	cfg, err := devconfig.Load(filepath.Join(home, "dev.json"))
	if err != nil {
		t.Fatalf("read the migrated config: %v", err)
	}
	if cfg.ContentCapture == nil || *cfg.ContentCapture {
		t.Errorf("content_capture:false was lost; migration did not run before the write (got %v)", cfg.ContentCapture)
	}
	if cfg.OrgSigningKeyID != "key-42" {
		t.Errorf("org signing pin was lost: %q", cfg.OrgSigningKeyID)
	}
	if cfg.Enforce == nil || *cfg.Enforce {
		t.Errorf("the enforce opt-out was lost across migration: %v", cfg.Enforce)
	}
	if _, err := os.Stat(filepath.Join(legacyDir, "dev.json")); err != nil {
		t.Errorf("the legacy config was removed: %v", err)
	}
}

func pointOSConfigDirAt(t *testing.T, dir string) {
	t.Helper()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("AppData", dir)
	case "darwin":
		t.Setenv("HOME", dir)
	default:
		t.Setenv("XDG_CONFIG_HOME", dir)
	}
}

func legacyConfigSubdir() string {
	if runtime.GOOS == "darwin" {
		return filepath.Join("Library", "Application Support", "openbox")
	}
	return "openbox"
}

// TestMigrationNoticeNamesWhatMoved the migration notice names both paths, so
// a user can see what moved.
func TestMigrationNoticeNamesWhatMoved(t *testing.T) {
	home := isolateHome(t)
	t.Setenv(devconfig.EnvConfigPath, "")
	legacyHome := t.TempDir()
	pointOSConfigDirAt(t, legacyHome)
	legacyDir := filepath.Join(legacyHome, legacyConfigSubdir())
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "dev.json"),
		[]byte(`{"developer_did":"did:aip:x"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	a, out, _ := testApp(nil)
	a.migrateLegacyConfig()
	s := out.String()
	if !strings.Contains(s, "Migrated") {
		t.Errorf("migration should be announced:\n%s", s)
	}
	if !strings.Contains(s, home) {
		t.Errorf("notice should name the destination:\n%s", s)
	}
	if !strings.Contains(s, "left in place") {
		t.Errorf("notice should say the original is kept:\n%s", s)
	}
}
