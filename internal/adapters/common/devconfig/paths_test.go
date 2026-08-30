package devconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHomeUsesOpenboxHomeOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvHome, dir)
	got, err := Home()
	if err != nil {
		t.Fatalf("Home(): %v", err)
	}
	if got != dir {
		t.Fatalf("Home() = %q, want %q", got, dir)
	}
}

// TestHomeRejectsRelativeOverride a relative OPENBOX_HOME would resolve
// against the process working directory, which for a hook is whatever project
// the tool is running in; the same config would resolve to a different file
// per session, and a credential write could land inside a repo.
func TestHomeRejectsRelativeOverride(t *testing.T) {
	t.Setenv(EnvHome, "relative/openbox")
	if _, err := Home(); err == nil {
		t.Fatal("Home() accepted a relative OPENBOX_HOME; want an error")
	}
}

func TestHomeDefaultsToDotOpenboxUnderHome(t *testing.T) {
	t.Setenv(EnvHome, "")
	home := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	} else {
		t.Setenv("HOME", home)
	}
	got, err := Home()
	if err != nil {
		t.Fatalf("Home(): %v", err)
	}
	if want := filepath.Join(home, ".openbox"); got != want {
		t.Fatalf("Home() = %q, want %q", got, want)
	}
}

// TestHomeDoesNotCreateTheDirectory home never creates anything: a read path
// must be able to ask where a file would be without making a directory as a
// side effect.
func TestHomeDoesNotCreateTheDirectory(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "not-yet")
	t.Setenv(EnvHome, dir)
	if _, err := Home(); err != nil {
		t.Fatalf("Home(): %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("Home() created %s; it must only be created by a write path", dir)
	}
}

func TestPathsDeriveFromHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvHome, dir)
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvApproverConfigPath, "")

	env, err := EnvFilePath()
	if err != nil {
		t.Fatalf("EnvFilePath(): %v", err)
	}
	if want := filepath.Join(dir, ".env"); env != want {
		t.Fatalf("EnvFilePath() = %q, want %q", env, want)
	}
	dev, err := DevConfigWritePath()
	if err != nil {
		t.Fatalf("DevConfigWritePath(): %v", err)
	}
	if want := filepath.Join(dir, "dev.json"); dev != want {
		t.Fatalf("DevConfigWritePath() = %q, want %q", dev, want)
	}
	appr, err := ApproverConfigWritePath()
	if err != nil {
		t.Fatalf("ApproverConfigWritePath(): %v", err)
	}
	if want := filepath.Join(dir, "approver.json"); appr != want {
		t.Fatalf("ApproverConfigWritePath() = %q, want %q", appr, want)
	}
}

// TestEnvConfigPathStillOverrides oPENBOX_CONFIG named the file directly
// before this layout existed; every operator override and existing test
// depends on it still winning.
func TestEnvConfigPathStillOverrides(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	explicit := filepath.Join(t.TempDir(), "elsewhere.json")
	t.Setenv(EnvConfigPath, explicit)

	if got := DefaultConfigPath(); got != explicit {
		t.Fatalf("DefaultConfigPath() = %q, want the OPENBOX_CONFIG override %q", got, explicit)
	}
	got, err := DevConfigWritePath()
	if err != nil {
		t.Fatalf("DevConfigWritePath(): %v", err)
	}
	if got != explicit {
		t.Fatalf("DevConfigWritePath() = %q, want %q", got, explicit)
	}
}

// TestReadFallsBackToLegacyConfigUntilMigrated the regression this guards:
// upgrading the binary must not silently ungovern a machine.
func TestReadFallsBackToLegacyConfigUntilMigrated(t *testing.T) {
	home := t.TempDir()
	legacyHome := t.TempDir()
	t.Setenv(EnvHome, home)
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvApproverConfigPath, "")
	pointUserConfigDirAt(t, legacyHome)

	legacyDev := filepath.Join(legacyConfigDir(), "dev.json")
	if err := os.MkdirAll(filepath.Dir(legacyDev), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyDev, []byte(`{"developer_did":"did:aip:legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := DefaultConfigPath(); got != legacyDev {
		t.Fatalf("DefaultConfigPath() = %q, want the legacy path %q while unmigrated", got, legacyDev)
	}
	w, err := DevConfigWritePath()
	if err != nil {
		t.Fatalf("DevConfigWritePath(): %v", err)
	}
	if w == legacyDev {
		t.Fatal("DevConfigWritePath() returned the legacy path; a write must never re-entrench it")
	}

	newDev := filepath.Join(home, "dev.json")
	if err := os.WriteFile(newDev, []byte(`{"developer_did":"did:aip:new"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DefaultConfigPath(); got != newDev {
		t.Fatalf("DefaultConfigPath() = %q, want the new path %q once it exists", got, newDev)
	}
}

func TestReadPrefersNewWhenNeitherExists(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvHome, home)
	t.Setenv(EnvConfigPath, "")
	pointUserConfigDirAt(t, t.TempDir())

	if want := filepath.Join(home, "dev.json"); DefaultConfigPath() != want {
		t.Fatalf("DefaultConfigPath() = %q, want %q when no config exists anywhere",
			DefaultConfigPath(), want)
	}
}

func TestLegacyConfigPathsNamesSecretsJSON(t *testing.T) {
	pointUserConfigDirAt(t, t.TempDir())
	dev, appr, secrets := LegacyConfigPaths()
	for _, p := range []string{dev, appr, secrets} {
		if p == "" {
			t.Fatal("LegacyConfigPaths returned an empty path")
		}
	}
	if !strings.HasSuffix(secrets, "secrets.json") {
		t.Fatalf("secrets path = %q, want it to end in secrets.json", secrets)
	}
}

func pointUserConfigDirAt(t *testing.T, dir string) {
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
