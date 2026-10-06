package hookflow

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAtomicWritesKeepModes the three records renameio now writes must stay
// 0600 under a 0700 session dir.
func TestAtomicWritesKeepModes(t *testing.T) {
	root := t.TempDir()

	t.Run("duration stash", func(t *testing.T) {
		d := DurationStash{Dir: filepath.Join(root, "durations")}
		if err := d.PutStart("sess-1", "key-1", "2026-08-27T00:00:00Z"); err != nil {
			t.Fatalf("PutStart: %v", err)
		}
		assertMode(t, d.RecordPath("sess-1", "key-1"), 0o600)
		assertMode(t, d.SessionDir("sess-1"), 0o700|os.ModeDir)

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
