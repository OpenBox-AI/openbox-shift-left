package sidecar

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

func TestServe_BindsServesAndShutsDown(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "run", "sidecar.sock")
	bundle := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(bundle, []byte(`{"version":"vT","rules":[
		{"id":"b","match":{"tool_name":"Bash","attribute_contains":{"command":"danger"}},"decision":"block","reason":"nope"}
	]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Config{
			SocketPath:   socket,
			BundlePath:   bundle,
			SyncInterval: 20 * time.Millisecond,
		})
	}()

	// Wait for the socket to appear (Serve binds before the first sync).
	waitFor(t, func() bool { _, err := os.Stat(socket); return err == nil }, 2*time.Second)

	// The socket dir is per-user 0700, the socket 0600 (INV-1).
	if fi, err := os.Stat(filepath.Dir(socket)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("socket dir perm = %v (err %v), want 0700", fiPerm(fi), err)
	}
	if fi, err := os.Stat(socket); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket perm = %v (err %v), want 0600", fiPerm(fi), err)
	}

	// Wait for the bundle to load out-of-band, then a real block decision proves
	// the full serve→sync→decide path.
	c := NewClient(ClientConfig{SocketPath: socket, Timeout: time.Second})
	waitFor(t, func() bool {
		d := c.Decide(context.Background(), toolCall("Bash", client.ToolShell, map[string]any{"command": "danger now"}))
		return !d.FailOpen && d.Evaluation.Verdict == client.VerdictBlock
	}, 2*time.Second)

	// Graceful shutdown on context cancel; the socket file is removed.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Errorf("socket not cleaned up after shutdown (err=%v)", err)
	}
}

func TestServe_RemovesStaleSocket(t *testing.T) {
	dir := t.TempDir()
	// t.TempDir() can be group-accessible depending on the host umask; tighten it
	// so the INV-1 socket-dir ownership check is satisfied (real usage binds under
	// a 0700 dir we create).
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "sidecar.sock")
	// Leave a stale socket file from a "previous crashed run".
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close() // closing removes it; recreate a bare file to simulate a leftover
	// Recreate as an actual socket leftover: bind then abandon without cleanup.
	ln2, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	// Do NOT close ln2 cleanly; instead just verify prepareSocketDir clears it.
	if err := prepareSocketDir(socket); err != nil {
		t.Fatalf("prepareSocketDir over stale socket: %v", err)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Errorf("stale socket not removed: %v", err)
	}
	_ = ln2.Close()
}

func TestPrepareSocketDir_RefusesNonSocketCollision(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { // pass the ownership check to reach the collision check
		t.Fatal(err)
	}
	path := filepath.Join(dir, "notasocket")
	if err := os.WriteFile(path, []byte("i am a regular file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareSocketDir(path); err == nil {
		t.Error("expected refusal to clobber a non-socket file, got nil")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("non-socket file should be left intact: %v", err)
	}
}

func TestPrepareSocketDir_RefusesGroupOrOtherAccessibleDir(t *testing.T) {
	// A pre-existing socket dir that grants group/other access is refused (INV-1,
	// G_SEC F1): another local user must not be able to reach the decision socket.
	dir := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // undo umask
		t.Fatal(err)
	}
	if err := prepareSocketDir(filepath.Join(dir, "sidecar.sock")); err == nil {
		t.Error("expected refusal of a 0755 socket dir, got nil")
	}
}

func TestPrepareSocketDir_AcceptsPrivateOwnedDir(t *testing.T) {
	// A 0700 dir we own is accepted.
	dir := filepath.Join(t.TempDir(), "tight")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := prepareSocketDir(filepath.Join(dir, "sidecar.sock")); err != nil {
		t.Errorf("private owned dir should be accepted: %v", err)
	}
}

func TestDefaultSocketPath_UsesXDGRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1234")
	if got := DefaultSocketPath(); got != "/run/user/1234/openbox/sidecar.sock" {
		t.Errorf("DefaultSocketPath() = %q", got)
	}
}

func waitFor(t *testing.T, cond func() bool, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", within)
}

func fiPerm(fi os.FileInfo) string {
	if fi == nil {
		return "<nil>"
	}
	return fi.Mode().Perm().String()
}
