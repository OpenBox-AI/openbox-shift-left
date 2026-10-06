//go:build windows

package atomicfile

import (
	"os"
	"path/filepath"
)

// Write writes data to path atomically, creating a new file with perm.
//
// The Sync is why this branch exists rather than deferring to os.WriteFile.
// renameio, which the other branch uses, quotes ext4's lead developer: without
// the fsync, "a zero-length file is a valid and possible outcome after the
// rename". This branch renamed without one, so the package promised on Windows
// a guarantee only Unix kept. The parent directory is not synced, matching
// renameio: losing that entry loses the rename, which leaves the previous file
// whole -- the lost update the package already bounds damage to.
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
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
