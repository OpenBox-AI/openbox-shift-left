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
// It also pins the direction a rewrite moves in, which renameio's default made
// subtle and wrong for these files: WriteFile applies WithExistingPermissions()
// after WithPermissions(), so a record that had become group- or world-readable
// kept those permissions through every later 0600 write. atomicWriteFile asks for
// WithStaticPermissions instead, so the requested mode is ENFORCED on a rewrite,
// not merely applied at creation — the behavior the hand-rolled writer this
// replaced had, and the one atomicwrite_windows.go still has.
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

	// The case a "does it stay 0600?" assertion cannot see. Preserving the mode
	// and enforcing it are the same thing on a file that is already 0600, so the
	// subtests above pass either way; only a WIDENED file tells them apart.
	t.Run("a widened record is tightened again", func(t *testing.T) {
		c := TurnCursor{Dir: filepath.Join(root, "widened")}
		if err := c.Write("sess-3", "agent-1", TurnPos{Offset: 1}); err != nil {
			t.Fatalf("Write: %v", err)
		}
		path := c.RecordPath("sess-3", "agent-1")
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatalf("chmod: %v", err)
		}

		if err := c.Write("sess-3", "agent-1", TurnPos{Offset: 2}); err != nil {
			t.Fatalf("Write rewrite: %v", err)
		}
		assertMode(t, path, 0o600)
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
