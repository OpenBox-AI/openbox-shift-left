//go:build darwin || linux

package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"golang.org/x/sys/unix"
)

func TestCopyIsDeterministicReadOnlyAndPreservesExecutableBit(t *testing.T) {
	parent := shortTempDir(t)
	root := filepath.Join(parent, "project")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeMode(t, filepath.Join(root, "a.txt"), []byte("alpha"), 0o644)
	writeMode(t, filepath.Join(root, "empty"), nil, 0o600)
	writeMode(t, filepath.Join(root, "bin", "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	if err := os.Symlink("a.txt", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "events.pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceModes := map[string]os.FileMode{
		"a.txt":      modeOf(t, filepath.Join(root, "a.txt")),
		"empty":      modeOf(t, filepath.Join(root, "empty")),
		"bin/run.sh": modeOf(t, filepath.Join(root, "bin", "run.sh")),
	}

	project, err := Resolve(root, Boundaries{})
	if err != nil {
		t.Fatal(err)
	}
	first := copyToPrivateDirectory(t, project, filepath.Join(parent, "snapshot-1"))
	second := copyToPrivateDirectory(t, project, filepath.Join(parent, "snapshot-2"))

	if string(first.Manifest()) != string(second.Manifest()) || first.Digest() != second.Digest() ||
		first.SelectionDigest() != second.SelectionDigest() || !reflect.DeepEqual(first.Files(), second.Files()) {
		t.Fatalf("repeated copy identities differ:\n%s\n%s", first.Manifest(), second.Manifest())
	}
	if strings.Contains(string(first.Manifest()), parent) || strings.Contains(string(first.Manifest()), first.Root()) {
		t.Fatalf("volatile absolute path leaked into manifest: %s", first.Manifest())
	}
	if first.FileCount() != 3 || first.TotalBytes() != int64(len("alpha")+len("#!/bin/sh\nexit 0\n")) {
		t.Fatalf("snapshot totals = %d files, %d bytes", first.FileCount(), first.TotalBytes())
	}
	gotExecutable := make(map[string]bool)
	for _, file := range first.Files() {
		gotExecutable[file.Path] = file.Executable
	}
	if !reflect.DeepEqual(gotExecutable, map[string]bool{"a.txt": false, "bin/run.sh": true, "empty": false}) {
		t.Fatalf("executable manifest = %#v", gotExecutable)
	}
	if got := artifact.DigestBytes(first.Manifest()); got != first.Digest() {
		t.Fatalf("manifest digest = %s, want %s", first.Digest(), got)
	}
	if _, err := os.Lstat(filepath.Join(first.Root(), "alias")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("internal symlink was copied: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(first.Root(), "events.pipe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("FIFO was copied: %v", err)
	}
	assertPermissions(t, first.Root(), 0o500)
	assertPermissions(t, filepath.Join(first.Root(), "bin"), 0o500)
	assertPermissions(t, filepath.Join(first.Root(), "a.txt"), 0o400)
	assertPermissions(t, filepath.Join(first.Root(), "empty"), 0o400)
	assertPermissions(t, filepath.Join(first.Root(), "bin", "run.sh"), 0o500)
	if file, err := os.OpenFile(filepath.Join(first.Root(), "a.txt"), os.O_WRONLY, 0); err == nil {
		_ = file.Close()
		t.Fatal("copied non-executable file remained writable")
	}
	if file, err := os.OpenFile(filepath.Join(first.Root(), "bin", "run.sh"), os.O_WRONLY, 0); err == nil {
		_ = file.Close()
		t.Fatal("copied executable file remained writable")
	}
	for relative, want := range sourceModes {
		if got := modeOf(t, filepath.Join(root, filepath.FromSlash(relative))); got != want {
			t.Fatalf("source mode %s = %04o, want %04o", relative, got, want)
		}
	}
	if err := project.Verify(first); err != nil {
		t.Fatal(err)
	}
}

func TestDependencyCompleteCopyIsClosedOverNodeModules(t *testing.T) {
	parent := shortTempDir(t)
	root := filepath.Join(parent, "project")
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "pinned-package"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeMode(t, filepath.Join(root, "src.mjs"), []byte("import 'pinned-package'\n"), 0o644)
	writeMode(t, filepath.Join(root, "node_modules", "pinned-package", "index.js"), []byte("export default true\n"), 0o644)
	project, err := Resolve(root, Boundaries{})
	if err != nil {
		t.Fatal(err)
	}
	defaultDestination := filepath.Join(parent, "default")
	makePrivateDirectory(t, defaultDestination)
	defaultCopy, err := project.Copy(defaultDestination)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(defaultCopy.Root(), "node_modules", "pinned-package", "index.js")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default snapshot unexpectedly retained dependency: %v", err)
	}
	trustedDestination := filepath.Join(parent, "trusted")
	makePrivateDirectory(t, trustedDestination)
	trustedCopy, err := project.CopyWithDependencies(trustedDestination)
	if err != nil {
		t.Fatal(err)
	}
	dependency := filepath.Join(trustedCopy.Root(), "node_modules", "pinned-package", "index.js")
	if content, err := os.ReadFile(dependency); err != nil || string(content) != "export default true\n" {
		t.Fatalf("dependency-complete snapshot content=%q error=%v", content, err)
	}
	if err := project.Verify(trustedCopy); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyDetectsChangedSourceBytes(t *testing.T) {
	parent := shortTempDir(t)
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "same-size.txt")
	writeMode(t, path, []byte("before"), 0o644)
	project, err := Resolve(root, Boundaries{})
	if err != nil {
		t.Fatal(err)
	}
	before := copyToPrivateDirectory(t, project, filepath.Join(parent, "snapshot-before"))
	writeMode(t, path, []byte("after!"), 0o644)
	if err := project.Verify(before); err == nil {
		t.Fatal("changed source bytes unexpectedly verified")
	}
	after := copyToPrivateDirectory(t, project, filepath.Join(parent, "snapshot-after"))
	if before.Digest() == after.Digest() || before.SelectionDigest() == after.SelectionDigest() {
		t.Fatal("same-size source byte change did not alter snapshot identities")
	}
}

func TestCopyRejectsMutationBeforeVerification(t *testing.T) {
	parent := shortTempDir(t)
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "source.txt")
	writeMode(t, path, []byte("stable"), 0o644)
	project, err := Resolve(root, Boundaries{})
	if err != nil {
		t.Fatal(err)
	}
	project.afterCopy = func() error {
		return os.WriteFile(path, []byte("mutated"), 0o644)
	}
	destination := filepath.Join(parent, "snapshot")
	makePrivateDirectory(t, destination)
	if _, err := project.Copy(destination); err == nil || !strings.Contains(err.Error(), "source changed while copying") {
		t.Fatalf("copy mutation error = %v", err)
	}
}

func TestCopyBoundsAFileThatGrowsAfterOpen(t *testing.T) {
	parent := shortTempDir(t)
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "growing.txt")
	writeMode(t, path, []byte("selected"), 0o644)
	project, err := Resolve(root, Boundaries{})
	if err != nil {
		t.Fatal(err)
	}
	project.afterSourceOpen = func(relative string) error {
		file, err := os.OpenFile(filepath.Join(root, filepath.FromSlash(relative)), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, writeErr := file.Write([]byte("+"))
		return errors.Join(writeErr, file.Close())
	}
	destination := filepath.Join(parent, "snapshot")
	makePrivateDirectory(t, destination)
	if _, err := project.Copy(destination); err == nil || !strings.Contains(err.Error(), "changed while copying") {
		t.Fatalf("growing source error = %v", err)
	}
}

func TestCopyRejectsDestinationMutationBeforeSeal(t *testing.T) {
	parent := shortTempDir(t)
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeMode(t, filepath.Join(root, "source.txt"), []byte("stable"), 0o644)
	project, err := Resolve(root, Boundaries{})
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(parent, "snapshot")
	makePrivateDirectory(t, destination)
	project.afterCopy = func() error {
		copied := filepath.Join(destination, "source.txt")
		if err := os.Chmod(copied, 0o600); err != nil {
			return err
		}
		return os.WriteFile(copied, []byte("tamper"), 0o600)
	}
	if _, err := project.Copy(destination); err == nil || !strings.Contains(err.Error(), "does not match its manifest") {
		t.Fatalf("destination mutation error = %v", err)
	}
}

func TestCopyRejectsUnsafeDestination(t *testing.T) {
	parent := shortTempDir(t)
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeMode(t, filepath.Join(root, "source.txt"), []byte("source"), 0o644)
	project, err := Resolve(root, Boundaries{})
	if err != nil {
		t.Fatal(err)
	}

	nonempty := filepath.Join(parent, "nonempty")
	makePrivateDirectory(t, nonempty)
	writeMode(t, filepath.Join(nonempty, "existing"), nil, 0o600)
	inside := filepath.Join(root, "inside")
	makePrivateDirectory(t, inside)
	permissive := filepath.Join(parent, "permissive")
	if err := os.Mkdir(permissive, 0o755); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(parent, "destination-link")
	if err := os.Symlink(nonempty, symlink); err != nil {
		t.Fatal(err)
	}
	for name, destination := range map[string]string{
		"relative":   "relative",
		"nonempty":   nonempty,
		"inside":     inside,
		"permissive": permissive,
		"symlink":    symlink,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := project.Copy(destination); err == nil {
				t.Fatalf("Copy(%q) unexpectedly passed", destination)
			}
		})
	}
}

func TestSelectedByteBoundRejectsOverflow(t *testing.T) {
	entries := []Entry{
		{Path: "first", Kind: KindRegular, Size: maxContractInteger},
		{Path: "second", Kind: KindRegular, Size: 1},
	}
	if err := validateSelectedByteBounds(entries); err == nil {
		t.Fatal("signed-53 aggregate overflow unexpectedly passed")
	}
}

func TestSourceDirectoryIdentityScanFindsNestedAliasTarget(t *testing.T) {
	root := shortTempDir(t)
	nested := filepath.Join(root, "nested", "empty")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	project, err := Resolve(root, Boundaries{})
	if err != nil {
		t.Fatal(err)
	}
	var target unix.Stat_t
	if err := unix.Stat(nested, &target); err != nil {
		t.Fatal(err)
	}
	found, err := projectContainsDirectoryIdentity(project, target)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("nested source directory identity was not found")
	}
}

func copyToPrivateDirectory(t *testing.T, project *Project, destination string) *Snapshot {
	t.Helper()
	makePrivateDirectory(t, destination)
	snapshot, err := project.Copy(destination)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { makeTreeRemovableForTest(destination) })
	return snapshot
}

func makePrivateDirectory(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeMode(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func assertPermissions(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if got := modeOf(t, path); got != want {
		t.Fatalf("mode %s = %04o, want %04o", path, got, want)
	}
}

func makeTreeRemovableForTest(root string) {
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
}
