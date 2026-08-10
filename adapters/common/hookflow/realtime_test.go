package hookflow

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

func testTrigger(t *testing.T, enabled bool) (RealtimeTrigger, *[]*exec.Cmd) {
	t.Helper()
	var spawned []*exec.Cmd
	return RealtimeTrigger{
		Spool:    Spool{Dir: t.TempDir()},
		Provider: "claude-code",
		Self:     "/fake/openbox",
		Enabled:  func() bool { return enabled },
		Start:    func(c *exec.Cmd) error { spawned = append(spawned, c); return nil },
	}, &spawned
}

func TestRealtimeMaybe_ClaimsLockAndSpawns(t *testing.T) {
	tr, spawned := testTrigger(t, true)
	// Append first, as the real hook path does, so the spool dir exists.
	if err := tr.Spool.Append(ev("sess-1", "evt-1")); err != nil {
		t.Fatal(err)
	}
	tr.Maybe(discard(), "sess-1")

	if len(*spawned) != 1 {
		t.Fatalf("want 1 spawn, got %d", len(*spawned))
	}
	cmd := (*spawned)[0]
	if cmd.Path != "/fake/openbox" {
		t.Errorf("spawn path = %q, want injected Self", cmd.Path)
	}
	wantArgs := []string{"/fake/openbox", "hook", "claude-code", "flush"}
	if strings.Join(cmd.Args, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("argv = %v, want %v", cmd.Args, wantArgs)
	}
	var sessEnv string
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, EnvFlushSession+"=") {
			sessEnv = e
		}
	}
	if sessEnv != EnvFlushSession+"=sess-1" {
		t.Errorf("flusher env = %q, want session id via %s", sessEnv, EnvFlushSession)
	}
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		t.Error("flusher must be fully detached from the hook's stdio")
	}
	if _, err := os.Stat(tr.Spool.FlushLockPath("sess-1")); err != nil {
		t.Errorf("debounce lock not left in place: %v", err)
	}
}

func TestRealtimeMaybe_DebouncesFreshLock(t *testing.T) {
	tr, spawned := testTrigger(t, true)
	tr.Maybe(discard(), "sess-1") // claims lock, spawns
	tr.Maybe(discard(), "sess-1") // fresh lock → skip
	tr.Maybe(discard(), "sess-1")
	if len(*spawned) != 1 {
		t.Fatalf("burst must spawn once, got %d", len(*spawned))
	}
}

func TestRealtimeMaybe_TakesOverStaleLock(t *testing.T) {
	tr, spawned := testTrigger(t, true)
	tr.Window = 50 * time.Millisecond
	tr.Maybe(discard(), "sess-1")
	// Age the lock past the window (backdate mtime instead of sleeping).
	lock := tr.Spool.FlushLockPath("sess-1")
	old := time.Now().Add(-time.Second)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	tr.Maybe(discard(), "sess-1")
	if len(*spawned) != 2 {
		t.Fatalf("stale lock must be taken over, got %d spawns", len(*spawned))
	}
	info, err := os.Stat(lock)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(info.ModTime()) > tr.Window {
		t.Error("takeover must refresh the lock mtime")
	}
}

func TestRealtimeMaybe_DisabledIsZeroIO(t *testing.T) {
	tr, spawned := testTrigger(t, false)
	tr.Maybe(discard(), "sess-1")
	if len(*spawned) != 0 {
		t.Fatal("disabled gate must not spawn")
	}
	// Byte-identical to pre-realtime behavior: not even the spool dir exists.
	if _, err := os.Stat(tr.Spool.Dir); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(tr.Spool.Dir)
		if len(entries) != 0 {
			t.Errorf("disabled gate wrote files: %v", entries)
		}
	}
}

func TestRealtimeMaybe_EmptySessionIDSkips(t *testing.T) {
	tr, spawned := testTrigger(t, true)
	tr.Maybe(discard(), "")
	if len(*spawned) != 0 {
		t.Fatal("empty session id must not spawn")
	}
}

func TestRealtimeMaybe_RefusesToSpawnTestBinary(t *testing.T) {
	// Self unset → os.Executable() resolves to THIS test binary (`*.test`),
	// which the trigger must refuse to spawn (re-running the suite would be a
	// fork bomb, not a flusher) — and it must bail before any filesystem
	// write, so the spool dir stays untouched.
	var spawned []*exec.Cmd
	tr := RealtimeTrigger{
		Spool:    Spool{Dir: filepath.Join(t.TempDir(), "spool")},
		Provider: "claude-code",
		Enabled:  func() bool { return true },
		Start:    func(c *exec.Cmd) error { spawned = append(spawned, c); return nil },
	}
	tr.Maybe(discard(), "sess-1")
	if len(spawned) != 0 {
		t.Fatalf("must not spawn the test binary, spawned %v", spawned[0].Args)
	}
	if _, err := os.Stat(tr.Spool.Dir); !os.IsNotExist(err) {
		t.Error("guard-blocked run must not create the spool dir or a lockfile")
	}
}

func TestRealtimeMaybe_SpawnFailureReleasesLock(t *testing.T) {
	tr, _ := testTrigger(t, true)
	tr.Start = func(*exec.Cmd) error { return os.ErrPermission }
	tr.Maybe(discard(), "sess-1")
	if _, err := os.Stat(tr.Spool.FlushLockPath("sess-1")); !os.IsNotExist(err) {
		t.Error("failed spawn must release the lock so later hooks can retry")
	}
}

func TestFlushLockLifecycle(t *testing.T) {
	s := Spool{Dir: filepath.Join(t.TempDir(), "spool")}
	// Touch creates (including the dir) when absent…
	s.TouchFlushLock("sess-1")
	lock := s.FlushLockPath("sess-1")
	info1, err := os.Stat(lock)
	if err != nil {
		t.Fatalf("TouchFlushLock did not create the lock: %v", err)
	}
	// …and refreshes mtime when present.
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(lock, old, old)
	s.TouchFlushLock("sess-1")
	info2, _ := os.Stat(lock)
	if !info2.ModTime().After(info1.ModTime().Add(-time.Minute)) {
		t.Error("TouchFlushLock must refresh mtime")
	}
	s.ReleaseFlushLock("sess-1")
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Error("ReleaseFlushLock must remove the lock")
	}
}

func TestSpoolDrainsIgnoreFlushLock(t *testing.T) {
	s := Spool{Dir: t.TempDir()}
	if err := s.Append(ev("sess-1", "evt-1")); err != nil {
		t.Fatal(err)
	}
	s.TouchFlushLock("sess-1")
	if IsRecoveryFile(filepath.Base(s.FlushLockPath("sess-1"))) {
		t.Error(".flushlock must never read as a recovery file")
	}
	n, err := s.FlushAll(context.Background(), func(_ context.Context, _ client.DevEvent) error { return nil })
	if err != nil || n != 1 {
		t.Fatalf("FlushAll over a dir with a lockfile: n=%d err=%v, want 1,nil", n, err)
	}
	// The lock survives a drain (its lifecycle belongs to the flusher).
	if _, err := os.Stat(s.FlushLockPath("sess-1")); err != nil {
		t.Errorf("FlushAll must not consume the lockfile: %v", err)
	}
}
