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

	// The org-level layout is what an unbound process sees, and this case is
	// the proof it did not move when the per-tool seam landed. Assert the
	// precondition rather than assume it: a leaked bind from another test
	// would otherwise make every path below wrong in the same direction and
	// this case would still read as a layout change.
	if got := BoundProvider(); got != "" {
		t.Fatalf("BoundProvider() = %q at the start of the unbound case; a bind leaked from another test", got)
	}

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
	dev, secrets := LegacyConfigPaths()
	for _, p := range []string{dev, secrets} {
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

// TestBoundPathsAreScopedToTheTool the whole point of the seam: once a process
// knows which tool it acts for, every identity file it touches is that tool's.
// The OPENBOX_CONFIG half is the asymmetry the design turns on -- the override
// names a dev.json and has never reached .env, so it must not start doing so
// now that .env is per-tool.
func TestBoundPathsAreScopedToTheTool(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvHome, home)
	t.Setenv(EnvConfigPath, "")
	pointUserConfigDirAt(t, t.TempDir())

	// A legacy dev.json that the unbound read would find. A bound read must
	// not: per-tool identity has no legacy location to fall back to.
	legacyDev := filepath.Join(legacyConfigDir(), "dev.json")
	if err := os.MkdirAll(filepath.Dir(legacyDev), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyDev, []byte(`{"developer_did":"did:aip:legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	release, err := BindProvider("codex")
	if err != nil {
		t.Fatalf("BindProvider(codex): %v", err)
	}
	defer release()

	if got := BoundProvider(); got != "codex" {
		t.Fatalf("BoundProvider() = %q, want codex", got)
	}

	env, err := EnvFilePath()
	if err != nil {
		t.Fatalf("EnvFilePath(): %v", err)
	}
	if want := filepath.Join(home, "codex", ".env"); env != want {
		t.Fatalf("EnvFilePath() = %q, want %q", env, want)
	}
	write, err := DevConfigWritePath()
	if err != nil {
		t.Fatalf("DevConfigWritePath(): %v", err)
	}
	if want := filepath.Join(home, "codex", "dev.json"); write != want {
		t.Fatalf("DevConfigWritePath() = %q, want %q", write, want)
	}
	read, err := DevConfigPath()
	if err != nil {
		t.Fatalf("DevConfigPath(): %v", err)
	}
	if want := filepath.Join(home, "codex", "dev.json"); read != want {
		t.Fatalf("DevConfigPath() = %q, want %q; a bound read must never reach the legacy location", read, want)
	}

	// OPENBOX_CONFIG shadows dev.json and only dev.json.
	explicit := filepath.Join(t.TempDir(), "elsewhere.json")
	t.Setenv(EnvConfigPath, explicit)
	if got, err := DevConfigPath(); err != nil || got != explicit {
		t.Fatalf("DevConfigPath() = %q (err %v), want the OPENBOX_CONFIG override %q even while bound", got, err, explicit)
	}
	env, err = EnvFilePath()
	if err != nil {
		t.Fatalf("EnvFilePath(): %v", err)
	}
	if want := filepath.Join(home, "codex", ".env"); env != want {
		t.Fatalf("EnvFilePath() = %q, want %q; OPENBOX_CONFIG must not shadow .env", env, want)
	}

	// The explicit accessors stay reachable from inside a bind: that is how
	// the enumerating commands read every store without binding in a loop.
	orgEnv, err := OrgEnvFilePath()
	if err != nil {
		t.Fatalf("OrgEnvFilePath(): %v", err)
	}
	if want := filepath.Join(home, ".env"); orgEnv != want {
		t.Fatalf("OrgEnvFilePath() = %q, want %q", orgEnv, want)
	}
	other, err := EnvFilePathFor("claude-code")
	if err != nil {
		t.Fatalf("EnvFilePathFor(claude-code): %v", err)
	}
	if want := filepath.Join(home, "claude-code", ".env"); other != want {
		t.Fatalf("EnvFilePathFor(claude-code) = %q, want %q", other, want)
	}
	otherCfg, err := DevConfigPathFor("claude-code")
	if err != nil {
		t.Fatalf("DevConfigPathFor(claude-code): %v", err)
	}
	if want := filepath.Join(home, "claude-code", "dev.json"); otherCfg != want {
		t.Fatalf("DevConfigPathFor(claude-code) = %q, want %q", otherCfg, want)
	}
	otherWrite, err := DevConfigWritePathFor("claude-code")
	if err != nil {
		t.Fatalf("DevConfigWritePathFor(claude-code): %v", err)
	}
	if want := filepath.Join(home, "claude-code", "dev.json"); otherWrite != want {
		t.Fatalf("DevConfigWritePathFor(claude-code) = %q, want %q", otherWrite, want)
	}
}

// TestBindReleaseRestoresThePreviousValue one process binds once, but a test
// binary dispatches many times; without a restoring release the first bind
// would decide every later case's identity.
func TestBindReleaseRestoresThePreviousValue(t *testing.T) {
	outer, err := BindProvider("codex")
	if err != nil {
		t.Fatalf("BindProvider(codex): %v", err)
	}
	defer outer()

	inner, err := BindProvider("claude-code")
	if err != nil {
		t.Fatalf("BindProvider(claude-code): %v", err)
	}
	if got := BoundProvider(); got != "claude-code" {
		t.Fatalf("BoundProvider() = %q, want claude-code", got)
	}
	inner()
	if got := BoundProvider(); got != "codex" {
		t.Fatalf("BoundProvider() = %q after release, want the restored codex", got)
	}
}

// TestBindRejectsAPathTraversalName the bound name becomes a directory
// component under ~/.openbox, so validation is the whole defence: a name
// carrying a separator or a dot element would write a credential outside the
// store it claims to be.
func TestBindRejectsAPathTraversalName(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	for _, name := range []string{"", "..", ".", "../..", "a/b", `a\b`, "codex/../..", " "} {
		before := BoundProvider()
		release, err := BindProvider(name)
		if err == nil {
			release()
			t.Fatalf("BindProvider(%q) was accepted; want an error", name)
		}
		if release != nil {
			t.Fatalf("BindProvider(%q) returned a release closure alongside its error", name)
		}
		if got := BoundProvider(); got != before {
			t.Fatalf("BindProvider(%q) changed the bound provider to %q on the error path", name, got)
		}
	}
}

// TestUnboundIdentityPathsAreTheOrgFiles the seam is inert until something
// binds; phase 05 is what makes an unbound .env read an error, and until then
// every existing caller must keep resolving exactly what it did before.
func TestUnboundIdentityPathsAreTheOrgFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvHome, home)
	t.Setenv(EnvConfigPath, "")

	release, err := BindProvider("codex")
	if err != nil {
		t.Fatalf("BindProvider(codex): %v", err)
	}
	release()

	env, err := EnvFilePath()
	if err != nil {
		t.Fatalf("EnvFilePath(): %v", err)
	}
	if want := filepath.Join(home, ".env"); env != want {
		t.Fatalf("EnvFilePath() = %q after release, want the org file %q", env, want)
	}
}
