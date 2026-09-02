//go:build darwin || linux

package snapshot

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestResolveProjectRootAndBoundaries(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "project-link")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	project, err := Resolve(alias, Boundaries{
		AuditOutput: filepath.Join(root, ".openbox", "audit", "run-1"),
		TempParent:  filepath.Join(root, ".openbox", "tmp"),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := canonicalExistingPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if project.Root() != resolvedRoot {
		t.Fatalf("resolved root = %q, want %q", project.Root(), resolvedRoot)
	}

	for name, path := range map[string]string{
		"filesystem root": string(filepath.Separator),
		"home":            mustHome(t),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Resolve(path, Boundaries{}); err == nil {
				t.Fatalf("Resolve(%q) unexpectedly passed", path)
			}
		})
	}
	if _, err := Resolve(root, Boundaries{AuditOutput: alias}); err == nil {
		t.Fatal("project root equal to audit output unexpectedly passed")
	}
	if _, err := Resolve(root, Boundaries{TempParent: root}); err == nil {
		t.Fatal("project root equal to temp parent unexpectedly passed")
	}
	if _, err := Resolve(root, Boundaries{TempParent: parent}); err == nil {
		t.Fatal("project root inside temp parent unexpectedly passed")
	}
}

func TestSelectDoesNotTraverseSymlinksOrRunBoundaries(t *testing.T) {
	parent := shortTempDir(t)
	root := filepath.Join(parent, "project")
	outside := filepath.Join(parent, "outside")
	for _, directory := range []string{
		root,
		outside,
		filepath.Join(root, "nested"),
		filepath.Join(root, ".openbox", "audit", "run-1"),
		filepath.Join(root, ".openbox", "tmp"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, filepath.Join(root, "a.txt"), "a", 0o755)
	writeFixture(t, filepath.Join(root, "nested", "b.txt"), "bb", 0o600)
	writeFixture(t, filepath.Join(root, "internal-target.txt"), "target", 0o600)
	writeFixture(t, filepath.Join(outside, "secret.txt"), "never inventory", 0o600)
	writeFixture(t, filepath.Join(root, ".openbox", "audit", "run-1", "ignored.txt"), "audit", 0o600)
	writeFixture(t, filepath.Join(root, ".openbox", "tmp", "ignored.txt"), "temp", 0o600)
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "external-file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "external-directory")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("internal-target.txt", filepath.Join(root, "internal-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(root, "broken-link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "named-pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(root, "local.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	project, err := Resolve(root, Boundaries{
		AuditOutput: filepath.Join(root, ".openbox", "audit", "run-1"),
		TempParent:  filepath.Join(root, ".openbox", "tmp"),
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := project.Select()
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, len(entries))
	byPath := make(map[string]Entry, len(entries))
	for index, entry := range entries {
		paths[index] = entry.Path
		byPath[entry.Path] = entry
	}
	wantPaths := []string{
		"a.txt",
		"broken-link",
		"external-directory",
		"external-file",
		"internal-link",
		"internal-target.txt",
		"local.sock",
		"named-pipe",
		"nested/b.txt",
	}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("selected paths = %#v, want %#v", paths, wantPaths)
	}
	assertEntry(t, byPath, "a.txt", KindRegular, "")
	if got := byPath["a.txt"].Mode.Perm(); got != 0o755 {
		t.Fatalf("a.txt mode = %04o", got)
	}
	if got := byPath["nested/b.txt"].Size; got != 2 {
		t.Fatalf("nested/b.txt size = %d", got)
	}
	assertEntry(t, byPath, "external-directory", KindExternalSymlink, "")
	assertEntry(t, byPath, "external-file", KindExternalSymlink, "")
	assertEntry(t, byPath, "internal-link", KindInternalSymlink, "internal-target.txt")
	assertEntry(t, byPath, "broken-link", KindBrokenSymlink, "")
	assertEntry(t, byPath, "local.sock", KindSocket, "")
	assertEntry(t, byPath, "named-pipe", KindFIFO, "")
	for _, excluded := range []string{
		".openbox/audit/run-1/ignored.txt",
		".openbox/inspect/prior-inspection/project-model.json",
		".openbox/tmp/ignored.txt",
		"secret.txt",
	} {
		if _, exists := byPath[excluded]; exists {
			t.Fatalf("excluded or external path %q was selected", excluded)
		}
	}
}

func TestSelectRejectsUnrepresentableAndUnsafePaths(t *testing.T) {
	t.Run("backslash", func(t *testing.T) {
		root := t.TempDir()
		writeFixture(t, filepath.Join(root, `bad\name`), "x", 0o600)
		project, err := Resolve(root, Boundaries{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := project.Select(); err == nil {
			t.Fatal("backslash path unexpectedly selected")
		}
	})

	t.Run("invalid UTF-8", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("Darwin rejects invalid UTF-8 path creation")
		}
		root := t.TempDir()
		name := string([]byte{'b', 'a', 'd', 0xff})
		writeFixture(t, filepath.Join(root, name), "x", 0o600)
		project, err := Resolve(root, Boundaries{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := project.Select(); err == nil {
			t.Fatal("invalid UTF-8 path unexpectedly selected")
		}
	})

	t.Run("root cycle symlink", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Symlink(".", filepath.Join(root, "cycle")); err != nil {
			t.Fatal(err)
		}
		project, err := Resolve(root, Boundaries{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := project.Select(); err == nil {
			t.Fatal("root-cycle symlink unexpectedly selected")
		}
	})
}

func TestSymlinkThatLeavesProjectIsAlwaysExternal(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(root, "target.txt"), "target", 0o600)
	if err := os.Symlink(filepath.Join(root, "target.txt"), filepath.Join(outside, "back")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "back"), filepath.Join(root, "direct-reentry")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "back"), filepath.Join(root, "hop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hop", filepath.Join(root, "via-internal")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "missing"), filepath.Join(root, "missing-external")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(root, "internal-hop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("internal-hop", filepath.Join(root, "internal-chain")); err != nil {
		t.Fatal(err)
	}
	project, err := Resolve(root, Boundaries{})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := project.Select()
	if err != nil {
		t.Fatal(err)
	}
	byPath := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		byPath[entry.Path] = entry
	}
	for _, external := range []string{"direct-reentry", "hop", "missing-external", "via-internal"} {
		assertEntry(t, byPath, external, KindExternalSymlink, "")
	}
	assertEntry(t, byPath, "internal-hop", KindInternalSymlink, "target.txt")
	assertEntry(t, byPath, "internal-chain", KindInternalSymlink, "target.txt")
}

func TestSelectRejectsDotDotAfterUnresolvedSymlinkComponent(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		target string
		setup  func(t *testing.T, root string)
	}{
		{
			name:   "missing component",
			target: "missing/../target.txt",
			setup:  func(*testing.T, string) {},
		},
		{
			name:   "external symlink component",
			target: "escape/../target.txt",
			setup: func(t *testing.T, root string) {
				outside := t.TempDir()
				if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			root := t.TempDir()
			writeFixture(t, filepath.Join(root, "target.txt"), "target", 0o600)
			fixture.setup(t, root)
			if err := os.Symlink(fixture.target, filepath.Join(root, "ambiguous")); err != nil {
				t.Fatal(err)
			}
			project, err := Resolve(root, Boundaries{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := project.Select(); err == nil {
				t.Fatal("dot-dot after unresolved component unexpectedly passed")
			}
		})
	}
}

func TestLeadingDotDotThatStaysInsideIsInternal(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(root, "target.txt"), "target", 0o600)
	if err := os.Symlink("../target.txt", filepath.Join(root, "nested", "link")); err != nil {
		t.Fatal(err)
	}
	project, err := Resolve(root, Boundaries{})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := project.Select()
	if err != nil {
		t.Fatal(err)
	}
	byPath := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		byPath[entry.Path] = entry
	}
	assertEntry(t, byPath, "nested/link", KindInternalSymlink, "target.txt")
}

func TestSelectUsesSchemaCharacterLimitForUnicodePath(t *testing.T) {
	root := t.TempDir()
	current, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	components := make([]string, 70)
	for index := range components {
		components[index] = strings.Repeat("界", 20) + fmt.Sprintf("%03d", index)
		if err := unix.Mkdirat(current, components[index], 0o700); err != nil {
			_ = unix.Close(current)
			t.Fatal(err)
		}
		next, err := unix.Openat(current, components[index], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
		if err != nil {
			_ = unix.Close(current)
			t.Fatal(err)
		}
		if err := unix.Close(current); err != nil {
			_ = unix.Close(next)
			t.Fatal(err)
		}
		current = next
	}
	leafFD, err := unix.Openat(current, "leaf.txt", unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC, 0o600)
	if err != nil {
		_ = unix.Close(current)
		t.Fatal(err)
	}
	leaf := os.NewFile(uintptr(leafFD), "leaf.txt")
	if _, err := leaf.Write([]byte("x")); err != nil {
		_ = leaf.Close()
		_ = unix.Close(current)
		t.Fatal(err)
	}
	if err := leaf.Close(); err != nil {
		_ = unix.Close(current)
		t.Fatal(err)
	}
	if err := unix.Close(current); err != nil {
		t.Fatal(err)
	}

	project, err := Resolve(root, Boundaries{})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := project.Select()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Kind != KindRegular {
		t.Fatalf("unicode-path selection = %#v", entries)
	}
	if len(entries[0].Path) <= 4096 {
		t.Fatalf("fixture path is only %d bytes", len(entries[0].Path))
	}
}

func assertEntry(t *testing.T, entries map[string]Entry, path string, kind Kind, target string) {
	t.Helper()
	entry, exists := entries[path]
	if !exists {
		t.Fatalf("missing entry %q", path)
	}
	if entry.Kind != kind || entry.LinkTarget != target {
		t.Fatalf("entry %q = %+v, want kind=%q target=%q", path, entry, kind, target)
	}
}

func writeFixture(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func mustHome(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return home
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "obx-snap-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func TestResolveMissingProjectFails(t *testing.T) {
	_, err := Resolve(filepath.Join(t.TempDir(), "missing"), Boundaries{})
	if err == nil || errors.Is(err, os.ErrExist) {
		t.Fatalf("missing project error = %v", err)
	}
}

func TestRegularVerificationOpenDoesNotBlockOnFIFO(t *testing.T) {
	root := shortTempDir(t)
	fifo := filepath.Join(root, "replacement")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	device, err := directoryDevice(directory)
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		file *os.File
		err  error
	}
	opened := make(chan result, 1)
	go func() {
		file, openErr := openRegularFile(directory, filepath.Base(fifo), device)
		opened <- result{file: file, err: openErr}
	}()
	select {
	case got := <-opened:
		if got.err != nil {
			t.Fatal(got.err)
		}
		defer got.file.Close()
		var stat unix.Stat_t
		if err := unix.Fstat(int(got.file.Fd()), &stat); err != nil {
			t.Fatal(err)
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFIFO {
			t.Fatalf("opened type = %#o, want FIFO", stat.Mode&unix.S_IFMT)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("regular-file verification open blocked on FIFO replacement")
	}
}
