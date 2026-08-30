package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestConcurrentInstallIsRefusedRatherThanQueued concurrent installs into one
// bundle each create a temp file, write the whole engine and rename, in a
// single directory; work apfs serializes on a lock for that directory.
func TestConcurrentInstallIsRefusedRatherThanQueued(t *testing.T) {
	pluginDir := t.TempDir()
	src := engineAt(t, t.TempDir(), "openbox", "engine-v1")

	const writers = 24
	var wg sync.WaitGroup
	errs := make([]error, writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start // release them together, so they genuinely contend
			errs[n] = Installer{
				PluginDir:    pluginDir,
				ConfigPath:   filepath.Join(t.TempDir(), "dev.json"),
				EngineBinary: src,
			}.Install(CredentialRef{DID: testDID})
		}(i)
	}
	close(start)
	wg.Wait()

	var ok, refused int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case strings.Contains(err.Error(), "already installing"):
			refused++
		default:
			t.Errorf("unexpected install error: %v", err)
		}
	}
	t.Logf("concurrent installs: %d proceeded, %d refused", ok, refused)
	if ok == 0 {
		t.Fatalf("every install was refused; at least one must proceed (refused=%d)", refused)
	}
	// The refusal itself is pinned deterministically by
	// TestStaleInstallLockIsReclaimed's fresh-lock case; what this test pins is
	// that contention never corrupts the bundle or strands the lock.
	if ok+refused != writers {
		t.Errorf("accounted %d of %d installs (ok=%d refused=%d)", ok+refused, writers, ok, refused)
	}
	got, err := os.ReadFile(filepath.Join(pluginDir, "bin", "openbox"))
	if err != nil {
		t.Fatalf("engine missing after concurrent installs: %v", err)
	}
	if string(got) != "engine-v1" {
		t.Errorf("engine = %q, want a complete copy", got)
	}
	if _, err := os.Stat(filepath.Join(pluginDir, ".install.lock")); !os.IsNotExist(err) {
		t.Errorf("install lock outlived the install (err=%v)", err)
	}
}

// TestStaleInstallLockIsReclaimed a lock left behind by a killed install must
// not wedge the bundle forever; that would turn a crash into a permanently un-
// installable state, which is a worse failure than the one the lock prevents.
func TestStaleInstallLockIsReclaimed(t *testing.T) {
	pluginDir := t.TempDir()
	src := engineAt(t, t.TempDir(), "openbox", "engine-v1")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(pluginDir, ".install.lock")
	if err := os.WriteFile(lock, []byte("99999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * installLockStale)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}

	inst := Installer{PluginDir: pluginDir, ConfigPath: filepath.Join(t.TempDir(), "dev.json"), EngineBinary: src}
	if err := inst.Install(CredentialRef{DID: testDID}); err != nil {
		t.Fatalf("a stale lock must be reclaimed, got: %v", err)
	}

	if err := os.WriteFile(lock, []byte("99999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := inst.Install(CredentialRef{DID: testDID})
	if err == nil || !strings.Contains(err.Error(), "already installing") {
		t.Errorf("a fresh lock must refuse the install, got: %v", err)
	}
}

func engineAt(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write engine %s: %v", name, err)
	}
	return path
}

// TestReInstallSkipsTheCopyWhenTheEngineIsUnchanged re-running `init` is
// expected; new hook keys only register on a re-init; so the common case is
// that the engine already in place is the one being installed.
func TestReInstallSkipsTheCopyWhenTheEngineIsUnchanged(t *testing.T) {
	src := engineAt(t, t.TempDir(), "openbox", "engine-v1")
	pluginDir := t.TempDir()
	inst := Installer{PluginDir: pluginDir, EngineBinary: src}

	if err := inst.placeEngineBinary(); err != nil {
		t.Fatalf("first place: %v", err)
	}
	dst := filepath.Join(pluginDir, "bin", "openbox")

	backdated := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(dst, backdated, backdated); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if err := inst.placeEngineBinary(); err != nil {
		t.Fatalf("second place: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat engine: %v", err)
	}
	if !info.ModTime().Equal(backdated) {
		t.Errorf("re-install rewrote an identical engine (mtime moved to %v); the copy must be skipped", info.ModTime())
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("skipped copy left a non-executable engine: %v", info.Mode())
	}
}

// TestReInstallReplacesTheEngineWhenTheBytesDiffer the skip must not be over-
// eager: a rebuilt engine has to land, or `init` silently keeps governing with
// the previous build.
func TestReInstallReplacesTheEngineWhenTheBytesDiffer(t *testing.T) {
	srcDir := t.TempDir()
	src := engineAt(t, srcDir, "openbox", "engine-v1")
	pluginDir := t.TempDir()

	if err := (Installer{PluginDir: pluginDir, EngineBinary: src}).placeEngineBinary(); err != nil {
		t.Fatalf("first place: %v", err)
	}
	newer := engineAt(t, srcDir, "openbox2", "engine-v2")
	if err := (Installer{PluginDir: pluginDir, EngineBinary: newer}).placeEngineBinary(); err != nil {
		t.Fatalf("second place: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(pluginDir, "bin", "openbox"))
	if err != nil {
		t.Fatalf("read engine: %v", err)
	}
	if string(got) != "engine-v2" {
		t.Errorf("engine = %q, want the rebuilt one; a same-size change was treated as unchanged", got)
	}
}

// TestPlaceEngineBinarySweepsAbandonedTempsAndSparesLiveOnes placeEngineBinary
// removes its own temp on every ordinary path via defer. What defer cannot
// survive is the process being killed, and a killed init leaves a multi-
// megabyte partial copy that nothing ever reclaimed.
func TestPlaceEngineBinarySweepsAbandonedTempsAndSparesLiveOnes(t *testing.T) {
	src := engineAt(t, t.TempDir(), "openbox", "engine-v1")
	pluginDir := t.TempDir()
	binDir := filepath.Join(pluginDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	abandoned := filepath.Join(binDir, ".openbox-abandoned.tmp")
	if err := os.WriteFile(abandoned, []byte("partial copy from a killed run"), 0o600); err != nil {
		t.Fatalf("seed abandoned temp: %v", err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(abandoned, old, old); err != nil {
		t.Fatalf("backdate abandoned temp: %v", err)
	}

	inFlight := filepath.Join(binDir, ".openbox-inflight.tmp")
	if err := os.WriteFile(inFlight, []byte("another install is mid-copy"), 0o600); err != nil {
		t.Fatalf("seed in-flight temp: %v", err)
	}
	foreign := filepath.Join(binDir, "keep-me.tmp")
	if err := os.WriteFile(foreign, []byte("not ours"), 0o600); err != nil {
		t.Fatalf("seed foreign file: %v", err)
	}
	if err := os.Chtimes(foreign, old, old); err != nil {
		t.Fatalf("backdate foreign file: %v", err)
	}

	if err := (Installer{PluginDir: pluginDir, EngineBinary: src}).placeEngineBinary(); err != nil {
		t.Fatalf("place: %v", err)
	}

	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Errorf("residue from a killed run was not reclaimed (err=%v)", err)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Errorf("a fresh temp was deleted; that breaks a concurrent install's rename: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("a file that is not ours was deleted: %v", err)
	}
}

// TestTheSweepRunsEvenWhenTheCopyIsSkipped the sweep has to run on the no-op
// path too, or a re-init against an unchanged engine; the common case; never
// reclaims anything.
func TestTheSweepRunsEvenWhenTheCopyIsSkipped(t *testing.T) {
	src := engineAt(t, t.TempDir(), "openbox", "engine-v1")
	pluginDir := t.TempDir()
	inst := Installer{PluginDir: pluginDir, EngineBinary: src}
	if err := inst.placeEngineBinary(); err != nil {
		t.Fatalf("first place: %v", err)
	}

	abandoned := filepath.Join(pluginDir, "bin", ".openbox-abandoned.tmp")
	if err := os.WriteFile(abandoned, []byte("residue"), 0o600); err != nil {
		t.Fatalf("seed abandoned temp: %v", err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(abandoned, old, old); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if err := inst.placeEngineBinary(); err != nil {
		t.Fatalf("second place: %v", err)
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Errorf("the skipped-copy path did not sweep residue (err=%v)", err)
	}
}
