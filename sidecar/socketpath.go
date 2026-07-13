package sidecar

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// socketDirPerm / socketFilePerm keep the decision socket PER-USER (INV-1): the
// containing dir is 0700 and the socket 0600, so no other local user can send
// the daemon a decision request or observe the tool-call metadata that crosses
// it. A world-accessible socket would let any local process influence
// enforcement or read INV-2 metadata.
const (
	socketDirPerm  os.FileMode = 0o700
	socketFilePerm os.FileMode = 0o600
	socketName                 = "sidecar.sock"
)

// DefaultSocketPath is the per-user path the daemon binds and the enforce hook
// dials. It prefers $XDG_RUNTIME_DIR (a per-user, tmpfs-backed, 0700 dir on
// Linux — the correct home for a user runtime socket); otherwise it falls back
// to a uid-scoped dir under the OS temp dir. Both the server and the Client call
// this, so they agree without configuration.
func DefaultSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "openbox", socketName)
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("openbox-%d", os.Getuid()), socketName)
}

// prepareSocketDir ensures the socket's parent dir exists and removes any stale
// socket file left by a previous crashed run (a leftover file would make
// net.Listen("unix", …) fail with "address already in use"). It does NOT remove
// a NON-socket file at the path — that would be an unexpected collision the
// operator should see, not something to silently clobber.
//
// Perms: the AUTHORITATIVE per-user access gate is the socket FILE (0600, set
// after bind in Serve) — on Linux, connect() to a Unix socket requires write on
// the socket file, so 0600 already limits it to the owner. The 0700 dir is
// defense-in-depth, and we only enforce it on a dir WE CREATE. A pre-existing
// parent (e.g. $XDG_RUNTIME_DIR, already 0700 by systemd, or a shared /tmp we do
// not own) is left untouched — chmod'ing someone else's dir would fail (root's
// /tmp) or overreach, and the 0600 socket makes it unnecessary.
func prepareSocketDir(path string) error {
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, socketDirPerm); err != nil {
			return fmt.Errorf("create socket dir %s: %w", dir, err)
		}
		// We created it → enforce 0700 (MkdirAll's perm is masked by umask).
		if err := os.Chmod(dir, socketDirPerm); err != nil {
			return fmt.Errorf("chmod socket dir %s: %w", dir, err)
		}
	}
	// Whether we just created it or it pre-existed (a prior run's dir, or the
	// non-XDG fallback under a world-writable /tmp), the socket dir MUST be owned
	// by THIS user and closed to group/other (INV-1). Otherwise a hostile local
	// user who pre-created (and thus owns) the dir could unlink our socket and
	// bind a rogue one at the same path — the enforce hook would then hand its
	// DecisionRequest (command/file metadata, and under E6-S4 the raw Content) to
	// the attacker and accept attacker-chosen verdicts (G_SEC F1). Refuse a dir we
	// do not exclusively own.
	if err := verifyOwnedAndPrivate(dir); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // clean slate
		}
		return fmt.Errorf("stat socket path %s: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket file at %s (unexpected collision)", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	return nil
}

// verifyOwnedAndPrivate refuses a socket dir that is not owned by THIS user, or
// that grants any group/other permission bit (INV-1, G_SEC F1). It is the guard
// against the non-XDG temp-dir hijack: a dir under a world-writable /tmp that an
// attacker pre-created (and thus owns) fails the uid check; a loosely-permissioned
// dir fails the mode check. The normal $XDG_RUNTIME_DIR/openbox path — owned by
// us at 0700 — passes.
func verifyOwnedAndPrivate(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat socket dir %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("socket dir %s is not a directory", dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot verify ownership of socket dir %s", dir)
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("refusing socket dir %s: owned by uid %d, not %d — "+
			"another local user could hijack the decision socket (INV-1)", dir, st.Uid, os.Getuid())
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("refusing socket dir %s: mode %#o grants group/other access; "+
			"the decision socket must be per-user (INV-1)", dir, perm)
	}
	return nil
}
