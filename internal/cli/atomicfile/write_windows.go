//go:build windows

package atomicfile

import (
	"os"
	"path/filepath"
)

// Write writes data to path atomically, creating a new file with perm.
//
// The Sync below is the whole point of this branch existing rather than
// deferring to os.WriteFile. renameio, which the other branch delegates to,
// quotes ext4's lead developer on why: without the fsync, "a zero-length file
// is a valid and possible outcome after the rename". This branch renamed
// without one for as long as it has existed, so the package's doc comment
// promised on Windows a guarantee only Unix was keeping.
//
// It syncs the temporary file and not the parent directory, which is exactly
// what renameio does on the other side. Losing the directory entry loses the
// rename, and losing the rename leaves the previous file whole -- a lost
// update, which is the damage bound the package claims and no more.
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
