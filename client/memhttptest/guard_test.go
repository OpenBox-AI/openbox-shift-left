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

	const self = "github.com/openbox-ai/openbox-shift-left/client/memhttptest"
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

// repoRoot walks up from this package to the directory holding go.work.
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
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("no go.work found above %s; the guard would scan the wrong tree", dir)
	return ""
}
