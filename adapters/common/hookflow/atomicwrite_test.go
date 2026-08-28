package hookflow

import (
	"os"
	"path/filepath"
	"testing"
)

// The three records renameio now writes must stay 0600 under a 0700 session dir.
//
// Asserted rather than assumed, because the swap moved file creation into a
// library: renameio creates its temp file with the permission passed to
// WriteFile, and if that argument were ever dropped the default is 0600 today but
// is the library's choice, not ours. These files live under ~/.openbox and a
// widening would be a real exposure — the duration stash and turn cursor are
// keyed by session and the findings cursor tracks an offset into an advisory
// file, none of which another local account should be able to rewrite.
//
// It also pins the property renameio's WithExistingPermissions() makes subtle: a
// REWRITE preserves the existing mode. A file that starts 0600 must not drift.
func TestAtomicWritesKeepModes(t *testing.T) {
	root := t.TempDir()

	t.Run("duration stash", func(t *testing.T) {
		d := DurationStash{Dir: filepath.Join(root, "durations")}
		if err := d.PutStart("sess-1", "key-1", "2026-08-27T00:00:00Z"); err != nil {
			t.Fatalf("PutStart: %v", err)
		}
		assertMode(t, d.RecordPath("sess-1", "key-1"), 0o600)
		assertMode(t, d.SessionDir("sess-1"), 0o700|os.ModeDir)

		// A rewrite must not widen it.
		if err := d.PutStart("sess-1", "key-1", "2026-08-27T00:00:01Z"); err != nil {
			t.Fatalf("PutStart rewrite: %v", err)
		}
		assertMode(t, d.RecordPath("sess-1", "key-1"), 0o600)
	})

	t.Run("turn cursor", func(t *testing.T) {
		c := TurnCursor{Dir: filepath.Join(root, "cursors")}
		if err := c.Write("sess-2", "agent-1", TurnPos{Offset: 42}); err != nil {
			t.Fatalf("Write: %v", err)
		}
		assertMode(t, c.RecordPath("sess-2", "agent-1"), 0o600)
		assertMode(t, c.SessionDir("sess-2"), 0o700|os.ModeDir)

		if err := c.Write("sess-2", "agent-1", TurnPos{Offset: 99}); err != nil {
			t.Fatalf("Write rewrite: %v", err)
		}
		assertMode(t, c.RecordPath("sess-2", "agent-1"), 0o600)
	})
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := fi.Mode(); got.Perm() != want.Perm() || got.IsDir() != want.IsDir() {
		t.Errorf("%s: mode %v, want %v", path, got, want)
	}
}
