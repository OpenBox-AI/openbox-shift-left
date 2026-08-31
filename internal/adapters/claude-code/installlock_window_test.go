package claudecode

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// TestInstallLockHoldsItsStalenessWindowAtBothEdges. The lock's whole purpose
// is the thousands-of-queued-installs incident its comment records, and the
// window that decides when a held lock becomes a dead one has never been
// tested at either edge: a minute of wall clock per assertion is not something
// a suite can afford, which is how the heuristic's own bugs survived. Fake
// time makes both edges cost nothing.
//
// The probes sit well clear of the edge because a filesystem may truncate
// mtime to whole seconds, which can only make time.Since read larger.
func TestInstallLockHoldsItsStalenessWindowAtBothEdges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		i := Installer{PluginDir: dir}
		lock := filepath.Join(dir, ".install.lock")

		release, err := i.acquireInstallLock()
		if err != nil {
			t.Fatalf("first acquire: %v", err)
		}
		if _, statErr := os.Stat(lock); statErr != nil {
			t.Fatalf("first acquire left no lock: %v", statErr)
		}
		// The create path stamps a real wall-clock mtime; inside a bubble
		// time.Now() is the fake clock, and comparing the two is meaningless.
		// The takeover path stamps with time.Now() itself, so it needs no help.
		now := time.Now()
		if err := os.Chtimes(lock, now, now); err != nil {
			t.Fatalf("stamp lock: %v", err)
		}

		time.Sleep(installLockStale / 2)
		if _, err := i.acquireInstallLock(); err == nil {
			t.Errorf("a second init %v into the %v window acquired the lock; that is the concurrent-install "+
				"pile-up the lock exists to prevent", installLockStale/2, installLockStale)
		} else if !strings.Contains(err.Error(), lock) {
			t.Errorf("refusal does not name the lock file, so a developer cannot clear it by hand: %v", err)
		}

		time.Sleep(2 * installLockStale)
		release2, err := i.acquireInstallLock()
		if err != nil {
			t.Errorf("a lock %v old was still treated as held: %v. A crashed installer would block every "+
				"later init until someone deleted the file by hand",
				installLockStale/2+2*installLockStale, err)
		}

		// The takeover restamped it, so the window runs from there.
		if _, err := i.acquireInstallLock(); err == nil {
			t.Error("the window did not restart after a takeover: a third init acquired the lock immediately")
		}

		release2()
		if _, statErr := os.Stat(lock); !os.IsNotExist(statErr) {
			t.Errorf("release left the lock in place: %v", statErr)
		}
		release()
	})
}

// TestInstallLockDoesNotBlockWhenItCannotLock preserves the fail-open branch,
// which is the one an installer notices only when it is gone: a plugin dir
// that cannot hold a lock file must not stop a legitimate install, so acquire
// returns a no-op release and no error rather than refusing. Phase 04 replaces
// the mechanism; this is what says the fallback survived it.
func TestInstallLockDoesNotBlockWhenItCannotLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not deny file creation on Windows")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil { // readable, not writable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if f, err := os.OpenFile(filepath.Join(dir, ".probe"), os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
		t.Skip("this user can write into a 0555 directory (root?); the branch is unreachable here")
	}

	i := Installer{PluginDir: dir}
	release, err := i.acquireInstallLock()
	if err != nil {
		t.Fatalf("acquire refused when it merely could not lock: %v\nINV-3 says a lock this code cannot "+
			"take must not become a reason to block an install", err)
	}
	if release == nil {
		t.Fatal("nil release on the fail-open path; the caller defers it unconditionally")
	}
	release() // must not panic, and must not remove a lock it never took
}
