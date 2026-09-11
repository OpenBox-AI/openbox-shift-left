package devconfig

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestResolve_ManyFlagsReadTheFilesOnce resolving one flag reads the managed
// config, its key set, the user config and its key set.
func TestResolve_ManyFlagsReadTheFilesOnce(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "dev.json")
	if err := os.WriteFile(cfg, []byte(`{"enforce":true,"tier2":true,"findings":true,"fail_closed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvConfigPath, cfg)
	t.Setenv(EnvManagedConfig, filepath.Join(dir, "absent.json"))

	before := ReadCount()
	for i := 0; i < 8; i++ {
		ResolveEnforce()
		ResolveFailClosed()
		ResolveTier2()
		ResolveFindings()
	}
	if got := ReadCount() - before; got > 2 {
		t.Errorf("32 flag resolutions caused %d file loads, want at most 2 (one per config file)", got)
	}
}

// TestResolve_FlagsShareOneViewOfTheFile the gate's flags must come from one
// version of the file. Resolving each one separately meant Enforce, FailClosed
// and Tier2 could each see a different dev.json if it were rewritten mid-hook,
// assembling a posture that never existed as a whole.
func TestResolve_FlagsShareOneViewOfTheFile(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "dev.json")
	write := func(body string) {
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"enforce":true,"tier2":true}`)
	t.Setenv(EnvConfigPath, cfg)
	t.Setenv(EnvManagedConfig, filepath.Join(dir, "absent.json"))

	defer Pin()()
	enforce := ResolveEnforce()
	write(`{"enforce":false,"tier2":false}`) // rewritten between resolutions
	tier2 := ResolveTier2()

	if enforce != tier2 {
		t.Errorf("enforce=%t but tier2=%t; the two flags came from different versions of dev.json", enforce, tier2)
	}
}

// TestResolve_RewrittenFileIsPickedUp a rewritten file must still be picked
// up, or the cache would pin a stale posture for the life of the process.
func TestResolve_RewrittenFileIsPickedUp(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "dev.json")
	t.Setenv(EnvConfigPath, cfg)
	t.Setenv(EnvManagedConfig, filepath.Join(dir, "absent.json"))

	if err := os.WriteFile(cfg, []byte(`{"enforce":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !ResolveEnforce() {
		t.Fatal("enforce should be true from the first write")
	}

	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(cfg, []byte(`{"enforce":false,"tier2":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ResolveEnforce() {
		t.Error("a rewritten dev.json must be observed, not served from cache")
	}
}

// TestManaged_UnknownLockedNamesAreReported a `locked` entry that governs
// nothing is reported, whether it names no setting at all (`enforcee`, a
// typo) or names a deprecated key that parses but is never honoured
// (`tier2`). The consequence is identical -- the org is told a mandate is in
// force and it is not -- so both reach doctor's "these lock NOTHING" line.
func TestManaged_UnknownLockedNamesAreReported(t *testing.T) {
	dir := t.TempDir()
	managed := filepath.Join(dir, "managed.json")
	if err := os.WriteFile(managed, []byte(`{"enforce":true,"locked":["enforce","enforcee","tier2"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvManagedConfig, managed)
	t.Setenv(EnvConfigPath, filepath.Join(dir, "dev.json"))

	st := Managed()
	if !st.Readable {
		t.Fatalf("managed file should be readable: %+v", st)
	}
	want := []string{"enforcee", "tier2"}
	if !slices.Equal(st.UnknownLocked, want) {
		t.Errorf("UnknownLocked = %v, want %v", st.UnknownLocked, want)
	}
}

// TestDeadKeysWarnFromTheManagedLayer the deprecation warning is the only
// surface left that tells anyone a dead key is dead: the posture row stopped
// publishing `tier2` so it could not be mistaken for live governance. That
// makes the managed layer the one it most has to reach -- an org setting the
// key is the reader who believes it is governing something.
func TestDeadKeysWarnFromTheManagedLayer(t *testing.T) {
	dir := t.TempDir()
	managed := filepath.Join(dir, "managed.json")
	if err := os.WriteFile(managed, []byte(`{"tier2":true,"locked":["tier2"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvManagedConfig, managed)
	t.Setenv(EnvConfigPath, filepath.Join(dir, "dev.json"))
	os.Unsetenv(EnvTier2)
	os.Unsetenv(EnvTier2Timeout)
	os.Unsetenv(EnvRequireVerified)

	dead := deadKeysPresent()
	if !slices.Contains(dead, "`tier2`") {
		t.Errorf("deadKeysPresent = %v, want it to name `tier2` from the managed layer", dead)
	}
}
