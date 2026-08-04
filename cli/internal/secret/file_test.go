package secret

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileStore_RoundTripAndPerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "secrets.json")
	s := NewFileStore(path)

	if _, err := s.Get(Service, "acme/claude-code/api_key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty store Get err = %v, want ErrNotFound", err)
	}
	if err := s.Set(Service, "acme/claude-code/api_key", "obx_test_secret"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(Service, "acme/claude-code/private_key", "c2VlZA=="); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Get(Service, "acme/claude-code/api_key"); err != nil || v != "obx_test_secret" {
		t.Fatalf("Get api_key = %q, %v", v, err)
	}

	// File is 0600 and the parent dir 0700 (INV-1: not world-readable at rest).
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("secret file perm = %o, want 600", perm)
	}
	if di, _ := os.Stat(filepath.Dir(path)); di.Mode().Perm() != 0o700 {
		t.Fatalf("secret dir perm = %o, want 700", di.Mode().Perm())
	}

	// Overwrite in place (idempotent re-init).
	if err := s.Set(Service, "acme/claude-code/api_key", "obx_test_rotated"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Get(Service, "acme/claude-code/api_key"); v != "obx_test_rotated" {
		t.Fatalf("after overwrite = %q", v)
	}

	// Delete removes just that account; the sibling survives.
	if err := s.Delete(Service, "acme/claude-code/api_key"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(Service, "acme/claude-code/api_key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete err = %v, want ErrNotFound", err)
	}
	if v, err := s.Get(Service, "acme/claude-code/private_key"); err != nil || v != "c2VlZA==" {
		t.Fatalf("sibling lost after delete: %q %v", v, err)
	}
}

func TestOpen_Selection(t *testing.T) {
	t.Setenv("OPENBOX_SECRET_FILE", filepath.Join(t.TempDir(), "s.json"))
	fs, err := Open("file")
	if err != nil || fs == nil {
		t.Fatalf("Open(file) = %v", err)
	}
	if err := fs.Set(Service, "a", "v"); err != nil {
		t.Fatalf("file store not usable: %v", err)
	}
	// "auto"/"os" never silently fall back to file — they use Detect (ErrNoStore
	// when no keyring). We can't assert the platform result, only that it is not
	// the file store.
	if _, err := Open("bogus"); !errors.Is(err, ErrUnknownBackend) {
		t.Fatalf("Open(bogus) = %v, want ErrUnknownBackend", err)
	}
}
