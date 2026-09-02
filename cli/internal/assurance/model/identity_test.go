package model

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollectProjectIdentityIsTruthfulForNonGitRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := CollectProjectIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Name != "project" || identity.Git.Present || identity.Git.Head != nil || identity.Git.Dirty != nil {
		t.Fatalf("identity = %#v", identity)
	}
}

func TestCollectProjectIdentityLeavesGitStateUnknown(t *testing.T) {
	for _, marker := range []string{"directory", "file", "symlink"} {
		t.Run(marker, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "project")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			gitMarker := filepath.Join(root, ".git")
			var err error
			switch marker {
			case "directory":
				err = os.Mkdir(gitMarker, 0o700)
			case "file":
				err = os.WriteFile(gitMarker, []byte("gitdir: elsewhere\n"), 0o600)
			case "symlink":
				err = os.Symlink("elsewhere", gitMarker)
			}
			if err != nil {
				t.Fatal(err)
			}
			identity, err := CollectProjectIdentity(root)
			if err != nil {
				t.Fatal(err)
			}
			if !identity.Git.Present || identity.Git.Head != nil || identity.Git.Dirty != nil {
				t.Fatalf("identity = %#v", identity)
			}
		})
	}
}
