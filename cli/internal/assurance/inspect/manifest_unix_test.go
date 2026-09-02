//go:build darwin || linux

package inspect

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/snapshot"
	"golang.org/x/sys/unix"
)

func TestReadManifestsClosedDeterministicSet(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "must-not-run")
	files := map[string][]byte{
		"package.json":           []byte(fmt.Sprintf(`{"name":"root","scripts":{"postinstall":"touch %s"}}`, sentinel)),
		"package-lock.json":      []byte(`{"lockfileVersion":3}`),
		"workspace/package.json": []byte(`{"name":"workspace"}`),
		"pyproject.toml":         []byte("[project]\nname = \"fixture\"\n"),
		"requirements.txt":       []byte("openbox-sdk==1.0.1\n"),
		"requirements/dev.txt":   []byte("pytest==9.0.0\n"),
		"uv.lock":                []byte("version = 1\n"),
		"README.md":              []byte("not a manifest\n"),
		"unknown.lock":           []byte("not supported\n"),
	}
	copied := copyManifestFixture(t, files)
	manifests, err := ReadManifests(copied)
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{
		"package-lock.json", "package.json", "pyproject.toml", "requirements.txt",
		"requirements/dev.txt", "uv.lock", "workspace/package.json",
	}
	wantKinds := map[string]ManifestKind{
		"package-lock.json": KindPackageLock, "package.json": KindPackageJSON,
		"pyproject.toml": KindPyprojectTOML, "requirements.txt": KindRequirements,
		"requirements/dev.txt": KindRequirements, "uv.lock": KindUVLock,
		"workspace/package.json": KindPackageJSON,
	}
	if len(manifests) != len(wantPaths) {
		t.Fatalf("manifest count = %d, want %d", len(manifests), len(wantPaths))
	}
	for index, manifest := range manifests {
		if manifest.Path() != wantPaths[index] {
			t.Fatalf("manifest[%d] path = %q, want %q", index, manifest.Path(), wantPaths[index])
		}
		if manifest.Kind() != wantKinds[manifest.Path()] {
			t.Fatalf("manifest %q kind = %q, want %q", manifest.Path(), manifest.Kind(), wantKinds[manifest.Path()])
		}
		if manifest.Digest() != artifact.DigestBytes(files[manifest.Path()]) || !bytes.Equal(manifest.Bytes(), files[manifest.Path()]) {
			t.Fatalf("manifest %q bytes or digest changed", manifest.Path())
		}
	}
	returned := manifests[0].Bytes()
	returned[0] ^= 0xff
	if bytes.Equal(returned, manifests[0].Bytes()) {
		t.Fatal("Manifest.Bytes returned mutable retained storage")
	}
	if _, err := os.Lstat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("package script executed or sentinel lookup failed: %v", err)
	}
}

func TestReadManifestsSelectedMastraMVPClone(t *testing.T) {
	projectRoot := os.Getenv("OPENBOX_MASTRA_SDK_ROOT")
	if projectRoot == "" {
		t.Skip("set OPENBOX_MASTRA_SDK_ROOT to the qualified local clone for the MVP integration")
	}
	destinationParent := t.TempDir()
	destination := filepath.Join(destinationParent, "snapshot")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	project, err := snapshot.Resolve(projectRoot, snapshot.Boundaries{
		AuditOutput: filepath.Join(projectRoot, ".openbox", "audit", "current"),
		TempParent:  destinationParent,
	})
	if err != nil {
		t.Fatal(err)
	}
	copied, err := project.Copy(destination)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { makeManifestFixtureWritable(destination) })
	manifests, err := ReadManifests(copied)
	if err != nil {
		t.Fatal(err)
	}
	foundPackage := false
	foundLock := false
	for _, manifest := range manifests {
		switch manifest.Path() {
		case "package.json":
			foundPackage = manifest.Kind() == KindPackageJSON && bytes.Contains(manifest.Bytes(), []byte(`"name": "@openbox-ai/openbox-mastra-sdk"`))
		case "package-lock.json":
			foundLock = manifest.Kind() == KindPackageLock
		}
	}
	if !foundPackage || !foundLock {
		t.Fatalf("qualified Mastra root package/lock not found: package=%v lock=%v", foundPackage, foundLock)
	}
}

func TestReadManifestsRejectsMalformedAndBoundedInputs(t *testing.T) {
	tests := []struct {
		name    string
		files   func() map[string][]byte
		wantErr string
	}{
		{name: "malformed package JSON", files: func() map[string][]byte { return map[string][]byte{"package.json": []byte(`{`)} }, wantErr: "invalid package_json"},
		{name: "duplicate package key", files: func() map[string][]byte { return map[string][]byte{"package.json": []byte(`{"name":"a","name":"b"}`)} }, wantErr: "duplicate object name"},
		{name: "non-object package JSON", files: func() map[string][]byte { return map[string][]byte{"package.json": []byte(`[]`)} }, wantErr: "cannot unmarshal array"},
		{name: "invalid UTF-8 TOML", files: func() map[string][]byte { return map[string][]byte{"pyproject.toml": {0xff}} }, wantErr: "NUL-free UTF-8"},
		{name: "NUL requirements", files: func() map[string][]byte { return map[string][]byte{"requirements.txt": []byte("safe\x00hidden")} }, wantErr: "NUL-free UTF-8"},
		{name: "oversized file", files: func() map[string][]byte {
			return map[string][]byte{"package.json": bytes.Repeat([]byte(" "), int(maxManifestBytes)+1)}
		}, wantErr: "exceeds 1048576 bytes"},
		{name: "deep package JSON", files: deepManifestJSON, wantErr: "depth exceeds 64"},
		{name: "too many manifests", files: tooManyManifestFiles, wantErr: "count exceeds 128"},
		{name: "aggregate bytes", files: aggregateManifestFiles, wantErr: "bytes exceed 8388608 total"},
		{name: "deep manifest path", files: deepManifestFile, wantErr: "exceeds depth 64"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copied := copyManifestFixture(t, test.files())
			_, err := ReadManifests(copied)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestReadManifestsRejectsSnapshotMutationAndUnsafeReplacement(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(t *testing.T, copied *snapshot.Snapshot)
	}{
		{name: "same-size content change", mutate: func(t *testing.T, copied *snapshot.Snapshot) {
			path := filepath.Join(copied.Root(), "package.json")
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(`{"name":"b"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hard link", mutate: func(t *testing.T, copied *snapshot.Snapshot) {
			replaceManifestEntry(t, copied.Root(), func(path string) error {
				external := filepath.Join(t.TempDir(), "external.json")
				if err := os.WriteFile(external, []byte(`{"name":"a"}`), 0o600); err != nil {
					return err
				}
				return os.Link(external, path)
			})
		}},
		{name: "symlink", mutate: func(t *testing.T, copied *snapshot.Snapshot) {
			replaceManifestEntry(t, copied.Root(), func(path string) error {
				external := filepath.Join(t.TempDir(), "external.json")
				if err := os.WriteFile(external, []byte(`{"name":"a"}`), 0o600); err != nil {
					return err
				}
				return os.Symlink(external, path)
			})
		}},
		{name: "FIFO", mutate: func(t *testing.T, copied *snapshot.Snapshot) {
			replaceManifestEntry(t, copied.Root(), func(path string) error { return unix.Mkfifo(path, 0o600) })
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			copied := copyManifestFixture(t, map[string][]byte{"package.json": []byte(`{"name":"a"}`)})
			mutation.mutate(t, copied)
			if _, err := ReadManifests(copied); err == nil {
				t.Fatal("mutated manifest snapshot was accepted")
			}
		})
	}
}

func copyManifestFixture(t *testing.T, files map[string][]byte) *snapshot.Snapshot {
	t.Helper()
	projectRoot := t.TempDir()
	for relative, content := range files {
		full := filepath.Join(projectRoot, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	destinationParent := t.TempDir()
	destination := filepath.Join(destinationParent, "snapshot")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	project, err := snapshot.Resolve(projectRoot, snapshot.Boundaries{
		AuditOutput: filepath.Join(projectRoot, ".openbox", "audit", "current"),
		TempParent:  destinationParent,
	})
	if err != nil {
		t.Fatal(err)
	}
	copied, err := project.Copy(destination)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { makeManifestFixtureWritable(destination) })
	return copied
}

func replaceManifestEntry(t *testing.T, root string, replacement func(string) error) {
	t.Helper()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "package.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := replacement(path); err != nil {
		t.Fatal(err)
	}
}

func makeManifestFixtureWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		} else if entry.Type()&os.ModeSymlink == 0 {
			_ = os.Chmod(path, 0o600)
		}
		return nil
	})
}

func tooManyManifestFiles() map[string][]byte {
	files := make(map[string][]byte, maxManifestCount+1)
	for index := 0; index <= maxManifestCount; index++ {
		files[filepath.ToSlash(filepath.Join("workspaces", fmt.Sprintf("%03d", index), "package.json"))] = []byte(`{}`)
	}
	return files
}

func aggregateManifestFiles() map[string][]byte {
	files := make(map[string][]byte, 9)
	content := append([]byte(`{"x":"`), bytes.Repeat([]byte("x"), int(maxManifestBytes)-8)...)
	content = append(content, []byte(`"}`)...)
	for index := 0; index < 9; index++ {
		files[filepath.ToSlash(filepath.Join("workspaces", fmt.Sprintf("%02d", index), "package.json"))] = content
	}
	return files
}

func deepManifestFile() map[string][]byte {
	components := make([]string, maxManifestPathDepth)
	for index := range components {
		components[index] = "d"
	}
	return map[string][]byte{strings.Join(append(components, "package.json"), "/"): []byte(`{}`)}
}

func deepManifestJSON() map[string][]byte {
	content := append([]byte(`{"value":`), bytes.Repeat([]byte("["), maxManifestJSONDepth)...)
	content = append(content, bytes.Repeat([]byte("]"), maxManifestJSONDepth)...)
	content = append(content, '}')
	return map[string][]byte{"package.json": content}
}
