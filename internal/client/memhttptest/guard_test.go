package memhttptest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var skipDirs = map[string]bool{
	"." + "git":    true,
	"node_modules": true,
	"testdata":     true,
}

// TestMemhttptestStaysTestOnly is the tripwire the package doc promises.
func TestMemhttptestStaysTestOnly(t *testing.T) {
	root := repoRoot(t)

	const self = "github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	var offenders []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
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

// repoRoot the marker is the root go.mod.
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

// isRepoRoot reports whether dir holds the repository's root go.mod. It checks
// the module PATH rather than the file's mere existence, so the walk cannot
// stop at some unrelated module that happens to sit above the checkout.
func isRepoRoot(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "module github.com/openbox-ai/openbox-shift-left" {
			return true
		}
	}
	return false
}
