package model

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// CollectProjectIdentity derives only identity that is truthful from bounded
// filesystem reads. It detects a Git marker but does not execute Git or infer
// HEAD or dirty state.
func CollectProjectIdentity(root string) (ProjectIdentity, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return ProjectIdentity{}, fmt.Errorf("model: stat project root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ProjectIdentity{}, errors.New("model: project root is not a resolved directory")
	}
	identity := ProjectIdentity{Name: filepath.Base(filepath.Clean(root))}
	if !validName(identity.Name) {
		return ProjectIdentity{}, errors.New("model: project directory name is not representable by openbox.project-model/v1")
	}
	if _, err := os.Lstat(filepath.Join(root, ".git")); errors.Is(err, os.ErrNotExist) {
		return identity, nil
	} else if err != nil {
		return ProjectIdentity{}, fmt.Errorf("model: inspect Git marker: %w", err)
	}
	identity.Git.Present = true
	return identity, nil
}
