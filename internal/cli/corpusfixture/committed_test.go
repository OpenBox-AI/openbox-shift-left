package corpusfixture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Committed_test.go; the permanent gate on every fixture this repository
// ships. A hardcoded list is a gate that silently stops covering the thing it
// was added for the moment somebody adds a fixture next door; and this
// particular gate failing open is a credential in git history.

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

func corpusDirs(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules") {
			return filepath.SkipDir
		}
		if filepath.Base(path) == "corpus" && filepath.Base(filepath.Dir(path)) == "testdata" {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func TestCommittedFixturesCarryNoRealIdentity(t *testing.T) {
	dirs := corpusDirs(t)
	if len(dirs) == 0 {
		t.Fatal("no testdata/corpus directories found; this gate is covering nothing")
	}

	scanned := 0
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			scanned++
			for _, v := range Scan(raw) {
				t.Errorf("%s: %s", path, v)
			}
		}
	}
	if scanned == 0 {
		t.Fatalf("found %d corpus director(ies) but scanned no fixtures; the gate is not covering anything", len(dirs))
	}
	t.Logf("scanned %d fixture(s) across %d director(ies)", scanned, len(dirs))
}

// TestCommittedFixtureScanIsNotVacuous is the drill, run in-process rather
// than claimed: plant an unsanitized fixture where the gate looks and confirm
// it is reported.
func TestCommittedFixtureScanIsNotVacuous(t *testing.T) {
	dirs := corpusDirs(t)
	if len(dirs) == 0 {
		t.Fatal("no testdata/corpus directories found; the drill has nowhere to plant")
	}
	planted := filepath.Join(dirs[0], "zz-drill-unsanitized.json")
	body := []byte(`{"attributes":[{"key":"user.email","value":{"stringValue":"real.person@example-corp.com"}}]}`)
	if err := os.WriteFile(planted, body, 0o644); err != nil {
		t.Fatalf("plant: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(planted) })

	raw, err := os.ReadFile(planted)
	if err != nil {
		t.Fatalf("read planted: %v", err)
	}
	if v := Scan(raw); len(v) == 0 {
		t.Fatal("the committed-fixture scan found nothing in a planted unsanitized fixture")
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

// TestCommittedFixtureScanCatchesRecordedProse is the second drill, and it
// exists because the rule it exercises is the one with a recognizer in front
// of it.
func TestCommittedFixtureScanCatchesRecordedProse(t *testing.T) {
	body := `{"model":"claude-x","messages":[{"role":"user","content":` +
		`[{"type":"text","text":"a verbatim recorded prompt that was never substituted"}]}]}`
	doc, err := json.Marshal(map[string]any{"request": map[string]any{"body": body}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if v := Scan(doc); len(v) == 0 {
		t.Fatal("the committed-fixture scan found nothing in a body carrying verbatim recorded prose")
	}
}
