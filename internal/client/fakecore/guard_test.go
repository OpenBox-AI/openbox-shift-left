package fakecore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFakecoreStaysTestOnly is the tripwire the package doc promises, and the
// second link of the chain memhttptest's own guard relies on: fakecore may
// import memhttptest from a non-_test.go file only because nothing in
// production imports fakecore. Break this and that exemption becomes a hole.
//
// It exists for a second reason of its own: fakecore answers verdicts a test
// scripted. A production path reaching it would be a governance decision made
// by a fixture.
func TestFakecoreStaysTestOnly(t *testing.T) {
	root := repoRoot(t)
	const self = "github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	skipDirs := map[string]bool{".git": true, "node_modules": true, "testdata": true}

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
		// fakecore's own files name the package in their import path only
		// incidentally; they are the package.
		if strings.HasPrefix(filepath.Dir(path), filepath.Join(root, "internal", "client", "fakecore")) {
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
		t.Errorf("%s is a NON-TEST file importing fakecore. fakecore serves verdicts a test scripted and replaces the process transport; production must never reach it.", o)
	}
}

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

// isRepoRoot checks the module PATH, not the file's mere existence, so the
// walk cannot stop at an unrelated module sitting above the checkout.
func isRepoRoot(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "module github.com/openbox-ai/openbox-shift-left")
}
