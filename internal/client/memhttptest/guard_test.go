package memhttptest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// skipDirs are directories the scan does not descend into. Named at runtime so
// the literal does not appear in the source.
var skipDirs = map[string]bool{
	"." + "git":    true,
	"node_modules": true,
	"testdata":     true,
}

// TestMemhttptestStaysTestOnly is the tripwire the package doc promises.
//
// This package is a non-test package in the shipped `client` module, and on
// first use it mutates `http.DefaultTransport` process-wide. `internal/` is not
// available to keep it honest — six modules import it, so it has to be exported
// — which leaves this check as the only thing standing between a test helper and
// production code that silently reroutes every outbound dial.
//
// Only NON-TEST files are an error. Every _test.go importer is the point.
func TestMemhttptestStaysTestOnly(t *testing.T) {
	root := repoRoot(t)

	const self = "github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	var offenders []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree must not silently shrink the scan.
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), self) {
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	for _, o := range offenders {
		t.Errorf("%s is a NON-TEST file importing memhttptest. This package replaces http.DefaultTransport for the whole process; production code must never reach it.", o)
	}
}

// The marker is the root go.mod. It used to be go.work, which was the only
// file true from every module while the repo had fifteen; the collapse to one
// module deleted it and left this walk climbing to the filesystem root.
//
// repoRoot walks up from this package to the directory holding the root go.mod.
//
// It fails rather than skips when it cannot find one: a guard that quietly
// passes because it scanned nothing is worse than no guard, which is the same
// reason gateway's moduleSources refuses an empty file list.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if isRepoRoot(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("no root go.mod found above %s; the guard would scan the wrong tree", dir)
	return ""
}

// isRepoRoot reports whether dir holds the repository's root go.mod.
//
// It checks the module PATH rather than the file's mere existence, so the walk
// cannot stop at some unrelated module that happens to sit above the checkout.
func isRepoRoot(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	// Line-wise, not a prefix of the file: the root go.mod opens with a comment
	// block, so the module line is not at byte zero. A prefix check compiles,
	// passes its own review, and then walks past the repo root to "/".
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "module github.com/openbox-ai/openbox-shift-left" {
			return true
		}
	}
	return false
}
