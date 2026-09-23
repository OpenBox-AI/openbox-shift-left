package devconfigtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDevconfigtestStaysTestOnly fails when a non-test file imports this
// package: a production path able to plant a legacy DID would be a way to
// silently ungovern a tool.
func TestDevconfigtestStaysTestOnly(t *testing.T) {
	const self = "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig/devconfigtest"
	root := repoRoot(t)
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata":
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
		if strings.Contains(string(b), `"`+self+`"`) {
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	for _, o := range offenders {
		t.Errorf("%s is a non-test file importing devconfigtest; only tests may plant a legacy store", o)
	}
}

// repoRoot walks up to the directory holding this module's go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(b), "module github.com/openbox-ai/openbox-shift-left\n") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no repository go.mod found above the test directory")
		}
		dir = parent
	}
}
