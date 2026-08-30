package depguard

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// depguard holds the dependency guards.
//
// They bound the package subtree rather than the module: with one go.mod, a
// per-module allowlist would name every external dependency at once, so it would
// either fail outright or be widened to the union, which removes the control.
//
// Do not delete this package as unused. Every file here is a _test.go, so it has
// no importer and no possible importer, which makes it indistinguishable from
// dead code to any reachability analysis. Run these with -count=1:
// TestConformanceClosureIsReviewed shells out to `go list`, whose reads never
// enter the test log.

// repoPrefix is the module namespace this repo publishes under.
const repoPrefix = "github.com/openbox-ai/openbox-shift-left"

// imports is one subtree's import surface, split along the two axes the guards
// bound independently.
//
// Both halves are load-bearing and an earlier draft of this phase kept only the
// first. Four of the five allowlists being replaced contain repo-local entries; // gateway's contains nothing else; and telemetry's repo-local set is
// deliberately empty, which is the whole quarantine. A repo-local-blind guard
// would let internal/gateway import the package that reads ~/.openbox/.env while
// staying green.
type imports struct {
	external  []string
	repoLocal []string
}

// imports is one subtree's import surface, split along the two axes the guards
// bound independently.
//
// Both halves are load-bearing. gateway's allowlist contains nothing but
// repo-local entries and telemetry's repo-local set is deliberately empty,
// which is the whole quarantine; a repo-local-blind guard would let
// internal/gateway import the package that reads ~/.openbox/.env while staying
// green.
func subtreeImports(root, selfPrefix string) (imports, error) {
	var out imports
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return out, fmt.Errorf("depguard: %s is not a readable directory: %w", root, err)
	}
	ext, local := map[string]bool{}, map[string]bool{}
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Prune what is not module source. Every guarded subtree currently has
			// a git-ignored .claude/ directory on disk, and a stray .go file
			// dropped in one; an editor temp file, a scratch fixture, another
			// session's debris; would otherwise be attributed to the subtree and
			// fail the allowlist for a file the module never compiled. A guard
			// that fails on tooling debris gets disabled, which is the same
			// outcome as not having it.
			if skipDir[d.Name()] || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		// ImportsOnly: a file that fails to compile for an unrelated reason still
		// yields its imports, so the guard does not go quiet mid-edit.
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			return fmt.Errorf("depguard: parse %s: %w", path, perr)
		}
		for _, spec := range f.Imports {
			p, uerr := strconv.Unquote(spec.Path.Value)
			if uerr != nil {
				return fmt.Errorf("depguard: import path in %s: %w", path, uerr)
			}
			switch {
			case under(p, selfPrefix):
				// the subtree's own packages
			case under(p, repoPrefix):
				local[p] = true
			case strings.Contains(firstSegment(p), "."):
				// Go's own rule: a stdlib import path has no dot in its first
				// segment. gateway's credential guard uses the same test, so the
				// two agree by construction.
				ext[p] = true
			}
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	if files == 0 {
		return out, fmt.Errorf("depguard: no .go files under %s; the guard would pass vacuously", root)
	}
	out.external, out.repoLocal = sorted(ext), sorted(local)
	return out, nil
}

// skipDir names directories that hold no module source. Dot-directories are
// pruned by prefix; these are the ones without a leading dot.
var skipDir = map[string]bool{"testdata": true, "node_modules": true, "vendor": true}

// under reports whether importPath is prefix itself or a package beneath it.
//
// The slash boundary is a security property, not tidiness: without it a `…/gateway`
// entry would also admit `…/gatewayfoo`, which is the case
// TestSelfModuleExemptionIsNarrow exists to pin.
func under(importPath, prefix string) bool {
	return importPath == prefix || strings.HasPrefix(importPath, prefix+"/")
}

func firstSegment(p string) string { s, _, _ := strings.Cut(p, "/"); return s }

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// unallowed returns the paths in got that no allowlist entry admits.
//
// Membership is entry-OR-ENTRY-subpackage, because allowlists name module paths
// while imports name package paths. telemetry allows
// `go.opentelemetry.io/collector/pdata` and imports `pdata/plog`, `pdata/ptrace`,
// `pdata/pmetric`, `pdata/pcommon`; decision allows `gitleaks/v8` and imports
// `gitleaks/v8/{config,detect,report}`. Under equality every one of those is red
// on the first run, and the obvious "fix" of rewriting entries to package paths
// grows both lists, which is precisely what that decision forbids. Prefix
// matching is also what a `require` already meant.
func unallowed(got []string, allow map[string]bool) []string {
	var bad []string
	for _, p := range got {
		ok := false
		for entry := range allow {
			if under(p, entry) {
				ok = true
				break
			}
		}
		if !ok {
			bad = append(bad, p)
		}
	}
	return bad
}

// dead returns allowlist entries that nothing under the subtree imports.
//
// This is the second direction, and telemetry and transport each carry it today
// as TestAllowlistHasNoDeadEntries: an entry nobody uses is a standing claim of
// review with nothing behind it, and should be dropped rather than left.
func dead(got []string, allow map[string]bool) []string {
	var stale []string
	for entry := range allow {
		used := false
		for _, p := range got {
			if under(p, entry) {
				used = true
				break
			}
		}
		if !used {
			stale = append(stale, entry)
		}
	}
	sort.Strings(stale)
	return stale
}

// repoRoot walks up to the repository root.
//
// The marker is the root go.mod. It was go.work while the repo had fifteen
// modules; the only file true from every one of them -- and four guards here
// t.Fatalf'd on its absence, which is how the collapse surfaced them.//
// It FAILS rather than skips when it finds neither, for the same reason
// subtreeImports refuses an empty tree.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			// Line-wise, not a prefix of the file: the root go.mod opens with a
			// comment block, so the module line is not at byte zero.
			for _, line := range strings.Split(string(b), "\n") {
				if strings.TrimSpace(line) == "module "+repoPrefix {
					return dir
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no root go.mod above the working directory; the guards would scan the wrong tree")
		}
		dir = parent
	}
}
