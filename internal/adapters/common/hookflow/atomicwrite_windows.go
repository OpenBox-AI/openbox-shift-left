//go:build windows

package hookflow

import (
	"os"
	"path/filepath"
)

// atomicWriteFile the Sync is the whole reason this branch exists rather than
// deferring to os.WriteFile. renameio, which the other branch delegates to,
// quotes ext4's lead developer on why: without the fsync, "a zero-length file
// is a valid and possible outcome after the rename". This copy renamed without
// one, so the durability guarantee held on Unix and not on Windows -- the same
// asymmetry internal/cli/atomicfile had, in a second copy the phase that fixed
// the first one missed.
//
// The temporary file is synced and the parent directory is not, which is
// exactly what renameio does on the other side. Losing the directory entry
// loses the rename, and that leaves the previous file whole.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".atomic-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
