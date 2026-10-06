package gatewaytest

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

// TestGatewaytestStaysTestOnly is the tripwire this package's doc promises.
func TestGatewaytestStaysTestOnly(t *testing.T) {
	root := repoRoot(t)

	const self = "github.com/openbox-ai/openbox-shift-left/internal/gateway/gatewaytest"
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
	if len(offenders) != 0 {
		t.Errorf("production files import the test-only dial swap: %v", offenders)
	}
}

// TestDialHookStaysInternal is the other half. Gatewaytest is guarded by the
// walk above; the variable it mutates is guarded by Go itself, and this
// asserts the arrangement still holds; a future move of dialhook out of
// internal/ would let any module in the repository assign the dial with no
// tripwire at all.
func TestDialHookStaysInternal(t *testing.T) {
	root := repoRoot(t)
	const hook = "internal/gateway/internal/dialhook"
	if _, err := os.Stat(filepath.Join(root, hook)); err != nil {
		t.Fatalf("%s is gone; the dial is no longer protected by internal/: %v", hook, err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if isRepoRoot(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no root go.mod above %s", dir)
		}
		dir = parent
	}
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
