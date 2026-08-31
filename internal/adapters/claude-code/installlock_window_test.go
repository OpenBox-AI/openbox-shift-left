package claudecode

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The mtime staleness window this file used to test at both edges no longer
// exists. What replaced it is an advisory lock, and its guarantees are
// different in kind: liveness comes from the kernel rather than from comparing
// a file's timestamp against a clock, so there is no window to probe. The tests
// below are what the new mechanism actually promises.

// TestInstallLockRefusesRatherThanQueues. The incident the lock exists for is
// recorded in its own comment: past a few dozen concurrent installers the queue
// drained slower than it filled and the processes never exited. A blocking
// acquire rebuilds exactly that, so the acquire must be non-blocking and the
// second caller must be told no, immediately.
func TestInstallLockRefusesRatherThanQueues(t *testing.T) {
	dir := t.TempDir()
	i := Installer{PluginDir: dir}
	lock := filepath.Join(dir, ".install.lock")

	release, err := i.acquireInstallLock()
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()

	start := time.Now()
	if _, err := i.acquireInstallLock(); err == nil {
		t.Error("a second acquire succeeded while the first still held the lock; two installs would " +
			"write the same bundle at the same time")
	} else {
		if !strings.Contains(err.Error(), lock) {
			t.Errorf("refusal does not name the lock file: %v", err)
		}
		if strings.Contains(err.Error(), "delete") {
			t.Errorf("refusal still tells the developer to delete the lock by hand; the kernel releases "+
				"it on process exit, so that advice would now be wrong: %v", err)
		}
	}
	if waited := time.Since(start); waited > 250*time.Millisecond {
		t.Errorf("the refused acquire took %v; it must not wait, or a stampede queues instead of "+
			"shedding", waited)
	}
}

// TestInstallLockIsReleasedByClosingItRatherThanByBookkeeping. Process exit and
// closing the descriptor are the same kernel path -- an advisory lock is held
// per open file description and released on its last close -- so releasing it
// without any cleanup logic running is the in-process stand-in for `kill -9` on
// a mid-install `openbox init`. Under the old mtime heuristic the second
// acquire refused for a full minute after such a kill.
func TestInstallLockIsReleasedByClosingItRatherThanByBookkeeping(t *testing.T) {
	dir := t.TempDir()
	i := Installer{PluginDir: dir}

	release, err := i.acquireInstallLock()
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := i.acquireInstallLock(); err == nil {
		t.Fatal("precondition: a held lock must refuse")
	}

	release()

	release2, err := i.acquireInstallLock()
	if err != nil {
		t.Fatalf("acquire after release: %v\nthe lock outlived its holder, which turns a crashed "+
			"install into a permanently uninstallable bundle", err)
	}
	release2()
}

// TestInstallLockIgnoresTheFilesystemClock is the point of the change. A lock
// file with an ancient timestamp and nobody holding it is not a lock; a lock
// file with an ancient timestamp that somebody *is* holding still is. The old
// code got the first right and the second wrong -- it stole any lock older than
// a minute, so an install slower than that was overrun by the next one, and an
// NTP step could do it at any age.
func TestInstallLockIgnoresTheFilesystemClock(t *testing.T) {
	dir := t.TempDir()
	i := Installer{PluginDir: dir}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, ".install.lock")
	ancient := time.Now().Add(-10 * 365 * 24 * time.Hour)

	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(lock, ancient, ancient); err != nil {
		t.Fatal(err)
	}
	release, err := i.acquireInstallLock()
	if err != nil {
		t.Fatalf("an unheld lock file, however old, must not block an install: %v", err)
	}

	if err := os.Chtimes(lock, ancient, ancient); err != nil {
		t.Fatal(err)
	}
	if _, err := i.acquireInstallLock(); err == nil {
		t.Error("a held lock was taken because its file looked old. Age says nothing about whether a " +
			"process is still installing, which is the entire reason the heuristic was replaced")
	}
	release()
}

// TestInstallLockDoesNotBlockWhenItCannotLock preserves the fail-open branch,
// which is the one an installer notices only when it is gone: a plugin dir that
// cannot hold a lock file must not stop a legitimate install, so acquire
// returns a no-op release and no error rather than refusing.
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
