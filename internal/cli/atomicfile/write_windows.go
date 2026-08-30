//go:build windows

package atomicfile

import (
	"os"
	"path/filepath"
)

// Write writes data to path atomically, creating a new file with perm.
//
// The unix variant uses google/renameio, which additionally fsyncs the file and
// its directory. That package declares a !windows constraint on every file and is
// therefore empty here, so this is the pre-existing temp+rename: atomic against a
// concurrent reader, but the contents are not fsynced before the rename. Windows
// is compile-verified only in this repo, and this preserves the behavior it had.
func Write(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".openbox-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// CreateTemp makes it 0600; match the intended perm rather than silently
	// tightening a file other tools read.
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
