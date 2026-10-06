package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The evidence table is hand-maintained with a checker, not generated.
//
// Generated, it would have under-credited the repository and over-credited the
// change that introduced it: most claims are owned by tests that already
// existed, and a generator reading only the new grader registry would not have
// seen them. Doc-plus-checker is also how the layout gate and the phantom-flag
// gate already work here, and neither generates either.
//
// What the checker owns is the half a human cannot keep true by hand: that
// every citation still resolves, and that no grader has quietly stopped being
// mentioned.

// citation matches "`path/to/file_test.go` · `TestName`".
var citation = regexp.MustCompile("`([a-z0-9_/.-]+\\.go)` · `(Test[A-Za-z0-9_]+)`")

func coverageDoc(t *testing.T) (string, string) {
	t.Helper()
	root := repoRootFromTest(t)
	path := filepath.Join(root, "docs", "coverage.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return root, string(b)
}

// TestEvidenceTableCitationsResolve: renaming a test without updating the table
// fails CI. Without this the table reads as authority while pointing at
// nothing, which is worse than having no table.
func TestEvidenceTableCitationsResolve(t *testing.T) {
	root, doc := coverageDoc(t)
	matches := citation.FindAllStringSubmatch(doc, -1)
	if len(matches) == 0 {
		t.Fatal("the evidence table cites no tests at all; either the format changed or the table was emptied, and this check would pass vacuously")
	}
	for _, m := range matches {
		file, name := m[1], m[2]
		b, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Errorf("the evidence table cites %s, which does not exist", file)
			continue
		}
		if !strings.Contains(string(b), "func "+name+"(") {
			t.Errorf("the evidence table cites %s in %s; no such test is declared there", name, file)
		}
	}
}

// TestEveryGraderIsCited: a grader that stops being mentioned has stopped being
// evidence anyone can find. The registry is the source of truth for what the
// suite checks; the table is where a reader looks.
func TestEveryGraderIsCited(t *testing.T) {
	_, doc := coverageDoc(t)
	// Reference graders are excluded on purpose: they are the wrong answers,
	// kept executable, and citing them as evidence would misrepresent what
	// they are.
	registry := gradedScenarios()
	if len(registry) == 0 {
		t.Fatal("the grader registry is empty; this check would pass vacuously")
	}
	for _, gs := range registry {
		// By name. An earlier form of this check accepted "some test that owns
		// graders is cited", which every grader satisfied at once and which
		// therefore checked nothing.
		if !strings.Contains(doc, "`"+gs.grader.Name+"`") {
			t.Errorf("grader %q is registered but is named nowhere in the evidence table; it is checking something no reader can find", gs.grader.Name)
		}
	}
}

// TestEvidenceTablePrintsItsUnprovenClaims: a claim with no evidence is the one
// a reader most needs to see, so E0 and E3 rows are printed rather than
// omitted. A table that had quietly dropped them would look complete.
func TestEvidenceTablePrintsItsUnprovenClaims(t *testing.T) {
	_, doc := coverageDoc(t)
	for _, want := range []string{"**E0**", "**E3**"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the evidence table prints no %s row; unproven claims are being omitted rather than declared", want)
		}
	}
}

func repoRootFromTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil &&
			strings.Contains(string(b), "module github.com/openbox-ai/openbox-shift-left") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("no root go.mod found; the check would scan the wrong tree")
	return ""
}
