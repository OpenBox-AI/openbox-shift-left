package devconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// migrateEnv points Home() at a fresh dir and os.UserConfigDir() at another, so
// the legacy and new locations are both writable temp dirs.
func migrateEnv(t *testing.T) (newHome, legacyDir string) {
	t.Helper()
	newHome = t.TempDir()
	t.Setenv(EnvHome, newHome)
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvApproverConfigPath, "")
	pointUserConfigDirAt(t, t.TempDir())
	legacyDir = legacyConfigDir()
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return newHome, legacyDir
}

func TestMigrateLegacyConfigCopiesBothFiles(t *testing.T) {
	newHome, legacyDir := migrateEnv(t)
	devBody := []byte(`{"developer_did":"did:aip:abc","enforce":true}`)
	apprBody := []byte(`{"org_id":"acme"}`)
	if err := os.WriteFile(filepath.Join(legacyDir, "dev.json"), devBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "approver.json"), apprBody, 0o600); err != nil {
		t.Fatal(err)
	}

	migrated, err := MigrateLegacyConfig()
	if err != nil {
		t.Fatalf("MigrateLegacyConfig(): %v", err)
	}
	if len(migrated) != 2 {
		t.Fatalf("migrated = %v, want both files", migrated)
	}
	for name, want := range map[string][]byte{"dev.json": devBody, "approver.json": apprBody} {
		got, err := os.ReadFile(filepath.Join(newHome, name))
		if err != nil {
			t.Fatalf("read migrated %s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s content = %q, want %q", name, got, want)
		}
	}
}

// Non-destructive by decision: deleting the legacy file makes a rollback to an
// older binary lossy and buys nothing but tidiness.
func TestMigrateLeavesTheLegacyFileInPlace(t *testing.T) {
	_, legacyDir := migrateEnv(t)
	legacy := filepath.Join(legacyDir, "dev.json")
	if err := os.WriteFile(legacy, []byte(`{"developer_did":"did:aip:abc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateLegacyConfig(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy file was removed or altered: %v", err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	_, legacyDir := migrateEnv(t)
	if err := os.WriteFile(filepath.Join(legacyDir, "dev.json"), []byte(`{"developer_did":"a"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := MigrateLegacyConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("first run migrated %v, want dev.json", first)
	}
	second, err := MigrateLegacyConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("second run migrated %v, want nothing", second)
	}
}

// Presence of the new file always wins: a user who has already run the new
// binary has a current config, and re-copying a stale legacy file over it would
// revert whatever they last set.
func TestMigrateNeverOverwritesAnExistingNewFile(t *testing.T) {
	newHome, legacyDir := migrateEnv(t)
	current := []byte(`{"developer_did":"did:aip:current"}`)
	if err := os.WriteFile(filepath.Join(newHome, "dev.json"), current, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "dev.json"), []byte(`{"developer_did":"did:aip:stale"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	migrated, err := MigrateLegacyConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrated) != 0 {
		t.Fatalf("migrated = %v, want nothing when the new file already exists", migrated)
	}
	got, _ := os.ReadFile(filepath.Join(newHome, "dev.json"))
	if string(got) != string(current) {
		t.Fatalf("existing config was overwritten: %q", got)
	}
}

func TestMigrateNoLegacyFilesIsANoOp(t *testing.T) {
	newHome, _ := migrateEnv(t)
	migrated, err := MigrateLegacyConfig()
	if err != nil {
		t.Fatalf("MigrateLegacyConfig(): %v", err)
	}
	if len(migrated) != 0 {
		t.Fatalf("migrated = %v, want nothing", migrated)
	}
	// Nothing to migrate must not create the directory either.
	if entries, err := os.ReadDir(newHome); err == nil && len(entries) != 0 {
		t.Fatalf("a no-op migration wrote %v", entries)
	}
}

// An unreadable legacy file is surfaced rather than swallowed: the alternative is
// a silent fresh start that loses the user's posture with no sign it happened.
func TestMigrateSurfacesAnUnreadableLegacyFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0000 does not deny reads on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny reads")
	}
	_, legacyDir := migrateEnv(t)
	if err := os.WriteFile(filepath.Join(legacyDir, "dev.json"), []byte(`{}`), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateLegacyConfig(); err == nil {
		t.Fatal("MigrateLegacyConfig() swallowed an unreadable legacy config")
	}
}

// Someone who set OPENBOX_CONFIG has named the file they want and is not asking
// to be migrated.
func TestMigrateSkipsWhenAnExplicitPathIsSet(t *testing.T) {
	_, legacyDir := migrateEnv(t)
	if err := os.WriteFile(filepath.Join(legacyDir, "dev.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvConfigPath, filepath.Join(t.TempDir(), "explicit.json"))
	migrated, err := MigrateLegacyConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrated {
		if m == "dev.json" {
			t.Fatal("migrated dev.json despite an explicit OPENBOX_CONFIG")
		}
	}
}

// An operator pointing OPENBOX_HOME at the legacy directory would otherwise have
// the migration copy a file over itself.
func TestMigrateNoOpWhenHomeIsTheLegacyDir(t *testing.T) {
	pointUserConfigDirAt(t, t.TempDir())
	legacy := legacyConfigDir()
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"developer_did":"did:aip:same"}`)
	if err := os.WriteFile(filepath.Join(legacy, "dev.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvHome, legacy)
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvApproverConfigPath, "")

	migrated, err := MigrateLegacyConfig()
	if err != nil {
		t.Fatalf("MigrateLegacyConfig(): %v", err)
	}
	if len(migrated) != 0 {
		t.Fatalf("migrated = %v, want nothing when both paths are the same dir", migrated)
	}
	got, _ := os.ReadFile(filepath.Join(legacy, "dev.json"))
	if string(got) != string(body) {
		t.Fatalf("file was rewritten: %q", got)
	}
}
