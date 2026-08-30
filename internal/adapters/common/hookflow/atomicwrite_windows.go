//go:build windows

package hookflow

import (
	"os"
	"path/filepath"
)

// atomicWriteFile writes data to path atomically, creating it with perm.
//
// The unix build uses google/renameio, which additionally fsyncs. That package
// declares `//go:build !windows` on every file and is therefore empty here, so
// this is the pre-existing temp+rename: atomic against a concurrent reader, but
// the contents are not fsynced before the rename. Windows is build-verified only
// in this repo, and this keeps its behavior exactly as it was.
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
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
