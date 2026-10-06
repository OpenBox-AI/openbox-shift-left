package fakecore

import (
	"go/parser"
	"go/token"
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

// TestFakecoreKeepsItsImportWall is the structural half of "the fixture is the
// oracle".
//
// A grader that computed its expectation with the same function production
// uses would agree with production by construction -- a renamed key, a torn
// pair, a mis-tagged struct field would move the answer and the expectation
// together, and the grader would confirm the bug rather than catch it. The
// wall makes that unreachable rather than merely discouraged: fakecore may
// reach the standard library and the in-memory transport, and nothing else.
//
// What it costs is that vocabulary has to be restated here -- the four wire
// types, the forbidden keys. That restatement IS the oracle doing its job.
// What it must never buy back is a derivation: an activity id, a pair key, an
// approval key.
func TestFakecoreKeepsItsImportWall(t *testing.T) {
	const allowed = "github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	scanned := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		// Parsed, not string-matched, and test files are scanned too.
		// Matching the file text meant skipping every _test.go -- a guard test
		// names repository paths as data -- which left the wall unchecked for
		// exactly the files a mirrored oracle would be written in. The parser
		// sees imports and nothing else, so the two cannot be confused.
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		scanned++
		for _, spec := range f.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			if !strings.HasPrefix(path, "github.com/openbox-ai/openbox-shift-left/") || path == allowed {
				continue
			}
			t.Errorf("%s imports %s. fakecore is the oracle: reaching into the code it grades would let a renamed key or a broken derivation move the expectation and the answer together. Restate the contract here instead.", e.Name(), path)
		}
	}
	if scanned == 0 {
		t.Fatal("no Go files were scanned; the wall would pass vacuously")
	}
}
