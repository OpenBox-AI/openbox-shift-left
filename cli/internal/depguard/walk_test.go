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

// depguard holds the dependency guards that used to live in five modules'
// go.mod-reading tests.
//
// WHY THEY MOVED. Each guard read its own `go.mod` and asserted a short
// allowlist of direct requires. With one module that file names every external
// dependency in the repository at once, so every allowlist would either fail
// outright or be "fixed" by widening it to the union — which looks like a fix and
// removes the control. ADR-0023's premise is that transitive code is bounded at
// the module that took the dependency; one module means one bound, so the bound
// moves to the package subtree.
//
// WHY ONE PACKAGE AND NOT SIX. While the modules still exist, a helper shared
// across them would need a `require`, a `replace`, AND an allowlist entry in each
// of the six — the exact widening this file exists to prevent. So the guards walk
// subtrees by FILESYSTEM PATH from the repo root, which needs no module
// relationship at all. `cli/cmd/openbox/modulewiring_test.go` and
// `cli/internal/corpusfixture/committed_test.go` both already do this.
//
// The package is test-only by construction: every file here is a _test.go, so
// nothing it contains can be reached from production code.
//
// RUN THESE WITH -count=1. MEASURED 2026-08-30, NOT INFERRED.
//
// Go's test cache does not verify reads that land in a SIBLING WORKSPACE MODULE.
// The boundary is the module, not the package directory -- measured both ways: a
// violating file planted in the same module but a DIFFERENT PACKAGE invalidates
// correctly and the test re-runs; the same file planted in a sibling `go.work`
// module returns a stale `ok`, for a new file and a modified one alike.
//
// That is pre-existing and not specific to this package. Every guard in this repo
// that walks across module boundaries has it -- `client/memhttptest/guard_test.go`
// reproduces it exactly, and `gateway/gatewaytest/guard_test.go`,
// `cli/internal/corpusfixture/committed_test.go` and
// `cli/cmd/openbox/modulewiring_test.go` read the same way. CI is exposed too:
// `actions/setup-go` restores the build cache by default, and adding a file to
// `gateway/` does not change this package's build ID.
//
// AFTER THE COLLAPSE most of this closes on its own -- one module means every walk
// here is same-module, which the measurement above shows is tracked. What stays
// uncacheable-stale in any layout is a guard that SHELLS OUT:
// TestConformanceClosureIsReviewed runs `go list` as a child process, and a
// child's file reads never enter the test log. That is what `-count=1` in ci.yml
// is for afterwards; do not remove it to save CI minutes.

// repoPrefix is the module namespace this repo publishes under.
const repoPrefix = "github.com/openbox-ai/openbox-shift-left"

// imports is one subtree's import surface, split along the two axes the guards
// bound independently.
//
// Both halves are load-bearing and an earlier draft of this phase kept only the
// first. Four of the five allowlists being replaced contain REPO-LOCAL entries —
// gateway's contains nothing else — and telemetry's repo-local set is
// deliberately EMPTY, which is the whole quarantine. A repo-local-blind guard
// would let internal/gateway import the package that reads ~/.openbox/.env while
// staying green.
type imports struct {
	external  []string
	repoLocal []string
}

// subtreeImports returns the non-stdlib imports appearing anywhere under root,
// split into repo-local and external, with imports of the subtree itself
// dropped.
//
// Build constraints are deliberately NOT evaluated. go/build would filter a
// `//go:build windows` file out on darwin, and this repo ships six per-GOOS
// files; a guard blind to the other platform has a hole exactly where
// cross-compilation puts one.
//
// _test.go files ARE included. gateway's cross-check against go.mod was the only
// thing bounding its test-only dependencies, and deleting that check is only a
// dissolution rather than a loss if this walk covers what it covered.
//
// An empty tree is an ERROR, not a pass: a guard that scanned nothing reports the
// same thing as a guard that found nothing wrong. gateway's own moduleSources
// refuses an empty file list for this reason.
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
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
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
// Membership is entry-OR-ENTRY-SUBPACKAGE, because allowlists name MODULE paths
// while imports name PACKAGE paths. telemetry allows
// `go.opentelemetry.io/collector/pdata` and imports `pdata/plog`, `pdata/ptrace`,
// `pdata/pmetric`, `pdata/pcommon`; decision allows `gitleaks/v8` and imports
// `gitleaks/v8/{config,detect,report}`. Under equality every one of those is red
// on the first run, and the obvious "fix" — rewriting entries to package paths —
// grows both lists, which is precisely what ADR-0023 forbids. Prefix matching is
// also what a `require` already meant.
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
// The marker must survive phase 03: `go.work` is deleted by it, and four other
// guards in this repo currently t.Fatalf on exactly that. So either marker
// counts — the workspace file today, the root module file after — and one of the
// two is always true.
//
// It FAILS rather than skips when it finds neither, for the same reason
// subtreeImports refuses an empty tree.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			if strings.HasPrefix(string(b), "module "+repoPrefix+"\n") {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.work or root go.mod above the working directory; the guards would scan the wrong tree")
		}
		dir = parent
	}
}
