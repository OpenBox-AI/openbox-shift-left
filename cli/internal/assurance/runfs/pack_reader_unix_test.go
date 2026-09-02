//go:build darwin || linux

package runfs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
)

func TestVerifyPackReadsFinalizedExactObjectSet(t *testing.T) {
	pack, root, workspace := finalizedTestPack(t)
	verified, err := VerifyPack(root)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Digest() != pack.Digest() || verified.RoleCount() != len(pack.Objects()) {
		t.Fatalf("verified digest/count = %s/%d", verified.Digest(), verified.RoleCount())
	}
	content, ok := verified.Object(artifact.RoleProjectModel)
	if !ok {
		t.Fatal("verified pack omitted project model")
	}
	content[0] = '!'
	again, _ := verified.Object(artifact.RoleProjectModel)
	if again[0] == '!' {
		t.Fatal("verified object was not defensively copied")
	}
	t.Cleanup(func() { _ = removeOwnedTree(root, workspace.identity) })
}

func TestVerifyPackRejectsIncompleteChangedMissingExtraAndTruncated(t *testing.T) {
	t.Run("incomplete", func(t *testing.T) {
		workspace, err := Create(filepath.Join(t.TempDir(), "incomplete"))
		if err != nil {
			t.Fatal(err)
		}
		defer cleanupIncomplete(t, workspace)
		if _, err := VerifyPack(workspace.Root()); err == nil {
			t.Fatal("accepted incomplete workspace")
		}
	})

	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string, *artifact.Pack)
	}{
		{name: "changed", mutate: func(t *testing.T, root string, pack *artifact.Pack) {
			path := objectPath(root, pack.Objects()[0].Digest())
			mustChmod(t, path, 0o600)
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			content[0] ^= 1
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			mustChmod(t, path, 0o400)
		}},
		{name: "missing", mutate: func(t *testing.T, root string, pack *artifact.Pack) {
			sha := filepath.Join(root, "objects", "sha256")
			mustChmod(t, sha, 0o700)
			if err := os.Remove(objectPath(root, pack.Objects()[0].Digest())); err != nil {
				t.Fatal(err)
			}
			mustChmod(t, sha, 0o500)
		}},
		{name: "extra", mutate: func(t *testing.T, root string, _ *artifact.Pack) {
			sha := filepath.Join(root, "objects", "sha256")
			mustChmod(t, sha, 0o700)
			path := filepath.Join(sha, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
			if err := os.WriteFile(path, []byte(`{}`), 0o400); err != nil {
				t.Fatal(err)
			}
			mustChmod(t, sha, 0o500)
		}},
		{name: "truncated manifest", mutate: func(t *testing.T, root string, _ *artifact.Pack) {
			path := filepath.Join(root, ManifestName)
			mustChmod(t, path, 0o600)
			if err := os.Truncate(path, 8); err != nil {
				t.Fatal(err)
			}
			mustChmod(t, path, 0o400)
		}},
		{name: "extra root entry", mutate: func(t *testing.T, root string, _ *artifact.Pack) {
			mustChmod(t, root, 0o700)
			if err := os.WriteFile(filepath.Join(root, "extra"), []byte("extra"), 0o400); err != nil {
				t.Fatal(err)
			}
			mustChmod(t, root, 0o500)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			pack, root, workspace := finalizedTestPack(t)
			defer func() { _ = removeOwnedTree(root, workspace.identity) }()
			test.mutate(t, root, pack)
			if _, err := VerifyPack(root); err == nil {
				t.Fatal("accepted mutated pack")
			}
		})
	}
}

func TestCommittedPackFinalRecheckDetectsSameSizeInPlaceChange(t *testing.T) {
	pack, root, workspace := finalizedTestPack(t)
	defer func() { _ = removeOwnedTree(root, workspace.identity) }()
	opened, err := openOwnedRoot(root, workspace.identity)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	object := pack.Objects()[0]
	path := objectPath(root, object.Digest())
	_, _, err = readCommittedPackAtWithHook(opened, func() error {
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
		content := object.Bytes()
		content[0] ^= 1
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return err
		}
		return os.Chmod(path, 0o400)
	})
	if err == nil {
		t.Fatal("final recheck accepted an in-place same-size mutation")
	}
}

func TestReadDirectoryBoundedRejectsExcessWithoutUnboundedEnumeration(t *testing.T) {
	root := t.TempDir()
	for index := 0; index < 100; index++ {
		name := filepath.Join(root, fmt.Sprintf("entry-%03d", index))
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	directory, err := openDirectoryNoFollow(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if _, err := readDirectoryBounded(directory, 2); err == nil {
		t.Fatal("accepted directory over the entry bound")
	}
}

func finalizedTestPack(t *testing.T) (*artifact.Pack, string, *Workspace) {
	t.Helper()
	pack := testAssembledPack(t)
	root := filepath.Join(t.TempDir(), "pack")
	workspace, err := Create(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.WritePackObjects(pack); err != nil {
		t.Fatal(err)
	}
	if digest, err := workspace.FinalizePack(pack); err != nil || digest != pack.Digest() {
		t.Fatalf("finalize = %s, %v", digest, err)
	}
	return pack, root, workspace
}

func objectPath(root string, digest artifact.ContentDigest) string {
	return filepath.Join(root, "objects", "sha256", digest.String()[len("sha256:"):])
}

func mustChmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
