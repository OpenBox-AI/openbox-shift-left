//go:build unix

package workloadauth

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCacheFileIs0600EvenOverAPreexisting0644File is unix-only: Windows 0600
// is a documented no-op (docs/credentials-and-secrets.md), so this asserts a
// permission bit that platform cannot express.
func TestCacheFileIs0600EvenOverAPreexisting0644File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("pre-creating 0644 file: %v", err)
	}

	fc := &FileCache{Path: path}
	if err := fc.Store(Entry{AccessToken: "atk"}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 0600", perm)
	}
}
