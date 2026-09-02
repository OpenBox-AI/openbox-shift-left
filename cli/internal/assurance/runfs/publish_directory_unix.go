//go:build darwin || linux

package runfs

import (
	"errors"
	"os"
	"path/filepath"
)

// PublishDirectoryNoReplace atomically publishes one already-sealed
// same-parent directory and never replaces an existing target.
func PublishDirectoryNoReplace(staging, target string) error {
	stagingPath, err := filepath.Abs(staging)
	if err != nil {
		return err
	}
	targetPath, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	if filepath.Dir(stagingPath) != filepath.Dir(targetPath) || stagingPath == targetPath {
		return errors.New("runfs: publication requires distinct same-parent directories")
	}
	info, err := os.Lstat(stagingPath)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o500 {
		return errors.New("runfs: staging directory is not a sealed private directory")
	}
	if _, err := os.Lstat(targetPath); err == nil {
		return errors.New("runfs: publication target already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent, err := openDirectoryNoFollow(filepath.Dir(targetPath))
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := renameNoReplaceAt(parent, filepath.Base(stagingPath), parent, filepath.Base(targetPath)); err != nil {
		return err
	}
	return syncOpenDirectory(parent)
}
