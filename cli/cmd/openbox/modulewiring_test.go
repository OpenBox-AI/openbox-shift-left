package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// modulewiring_test.go — every intra-repo import is wired in the importer's own
// go.mod, with BOTH a require and a replace.
//
// THE DEFECT THIS EXISTS FOR. `cli/cmd/openbox/transport.go` imported
// `.../transport` while `cli/go.mod` declared neither. `go.work` resolved it, so
// the whole workspace was green — `go build`, `go vet`, `go test -race` across
// every module, both cross-compiles — while `GOWORK=off`, which is the ONLY path
// `.goreleaser.yaml` runs, could not resolve the import at all. The release
// artifact did not build, and nothing said so.
//
// TWO REASONS A GATE HAS TO BE STRUCTURAL RATHER THAN A HABIT:
//
//  1. `go mod tidy` does NOT fix it. Tidy writes `require` lines; it does not
//     write `replace` directives, and an intra-repo `v0.0.0` require with no
//     replace has no source to resolve from. So "just run tidy" is a remedy that
//     silently only half works — which is worse than none, because it looks done.
//  2. The workspace HIDES it by design. `go.work` exists so the repo can be built
//     as a whole; the cost is that it papers over exactly this class. A CI step
//     doing per-module `GOWORK=off go build` catches it too and has been added —
//     but that step needs a module cache and a network, and this one does not, so
//     this is the copy that runs on a developer's machine before the push.
//
// SCOPE. This checks the WIRING (require + replace present and pointing at a real
// directory), not the dependency policy. What a module is allowed to depend on is
// each module's own `guard_test.go` (ADR-0023) and is a different question.

// repoPrefix is the module namespace this repo publishes under.
const repoPrefix = "github.com/openbox-ai/openbox-shift-left/"

// TestEveryIntraRepoImportIsWiredInGoMod walks every module in the workspace.
//
// Repo-wide rather than cli-only: cli is where it broke, but the next module to
// grow an intra-repo import is the one this is for, and a gate that covers only
// the place that already failed covers the one case that cannot recur.
func TestEveryIntraRepoImportIsWiredInGoMod(t *testing.T) {
	root := repoRootFromHere(t)
	modules := modulesInWorkspace(t, root)
	if len(modules) < 10 {
		// A scan that found almost nothing would pass while measuring nothing —
		// the same reason gateway's moduleSources refuses an empty file list.
		t.Fatalf("found only %d modules under %s; this gate would be near-vacuous", len(modules), root)
	}

	// modulePathByDir and its inverse let an IMPORT be resolved to the module that
	// OWNS it. `client/memhttptest` is a package inside the `client` module, not a
	// module of its own, so wiring is owed for `client` — checking the import path
	// verbatim reported six false failures on the first run.
	owners := make([]string, 0, len(modules))
	for _, modDir := range modules {
		raw, err := os.ReadFile(filepath.Join(modDir, "go.mod"))
		if err != nil {
			t.Fatalf("read %s/go.mod: %v", modDir, err)
		}
		if p := modulePath(string(raw)); p != "" {
			owners = append(owners, p)
		}
	}

	checked := 0
	for _, modDir := range modules {
		gomodPath := filepath.Join(modDir, "go.mod")
		raw, err := os.ReadFile(gomodPath)
		if err != nil {
			t.Errorf("read %s: %v", gomodPath, err)
			continue
		}
		gomod := string(raw)
		selfPath := modulePath(gomod)

		for _, imp := range intraRepoImports(t, modDir) {
			if imp == selfPath || strings.HasPrefix(imp, selfPath+"/") {
				continue // a module importing its own subpackage needs no wiring
			}
			owner := owningModule(owners, imp)
			if owner == "" {
				t.Errorf("%s imports %s, which belongs to no module in go.work",
					relTo(root, modDir), imp)
				continue
			}
			imp = owner
			checked++

			if !declaresRequire(gomod, imp) {
				t.Errorf("%s imports %s but %s has no require for it.\n"+
					"go.work resolves this, so the whole workspace stays green while `GOWORK=off` — "+
					"the only path the release build runs — cannot resolve it.",
					relTo(root, modDir), imp, relTo(root, gomodPath))
			}
			target, ok := replaceTarget(gomod, imp)
			if !ok {
				t.Errorf("%s requires %s but %s has no replace directive for it.\n"+
					"An intra-repo v0.0.0 require has no source without one, and `go mod tidy` "+
					"does NOT write replace directives — so tidying is not the fix.",
					relTo(root, modDir), imp, relTo(root, gomodPath))
				continue
			}
			// A replace pointing nowhere fails the same way as a missing one, and
			// reads as wired.
			if _, err := os.Stat(filepath.Join(modDir, target, "go.mod")); err != nil {
				t.Errorf("%s replaces %s with %q, which has no go.mod: %v",
					relTo(root, gomodPath), imp, target, err)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no intra-repo imports were checked; the import extractor is broken and this gate is vacuous")
	}
	t.Logf("checked %d intra-repo import edges across %d modules", checked, len(modules))
}

// intraRepoImports returns the intra-repo module paths imported by the non-test
// AND test files of every package under modDir.
//
// Test files count. A test-only import still has to resolve under `GOWORK=off`
// for `go test` to run outside the workspace, and it is the same missing-replace
// failure one command over.
func intraRepoImports(t *testing.T, modDir string) []string {
	t.Helper()
	seen := map[string]bool{}
	err := filepath.WalkDir(modDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata":
				return filepath.SkipDir
			}
			// Do not descend into a NESTED module; it owns its own go.mod.
			if path != modDir {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// PARSED, not grepped. A regex over the file body also matches a repo path
		// written as a STRING LITERAL — decision/guard_test.go embeds a fake go.mod
		// naming `gateway` as fixture data, and the first version of this gate
		// reported it as an unwired import. Only the import declarations count.
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			return nil // unparseable file: not this gate's business to fail on
		}
		for _, spec := range f.Imports {
			imp, uerr := strconv.Unquote(spec.Path.Value)
			if uerr != nil || !strings.HasPrefix(imp, repoPrefix) {
				continue
			}
			seen[imp] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", modDir, err)
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	return out
}

// owningModule returns the workspace module that an import path belongs to,
// by longest matching prefix — so `client/memhttptest` resolves to `client`.
func owningModule(owners []string, imp string) string {
	best := ""
	for _, o := range owners {
		if imp == o || strings.HasPrefix(imp, o+"/") {
			if len(o) > len(best) {
				best = o
			}
		}
	}
	return best
}

// modulesInWorkspace reads go.work rather than globbing for go.mod, so a module
// deliberately kept out of the workspace is not judged by workspace rules.
func modulesInWorkspace(t *testing.T, root string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "go.work"))
	if err != nil {
		t.Fatalf("read go.work: %v", err)
	}
	var out []string
	inUse := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "use ("):
			inUse = true
		case inUse && line == ")":
			inUse = false
		case inUse && strings.HasPrefix(line, "./"):
			out = append(out, filepath.Join(root, line))
		case strings.HasPrefix(line, "use ./"):
			out = append(out, filepath.Join(root, strings.TrimPrefix(line, "use ")))
		}
	}
	return out
}

// modulePath returns the module's own path from its go.mod.
func modulePath(gomod string) string {
	for _, line := range strings.Split(gomod, "\n") {
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

// declaresRequire reports whether go.mod requires the path, in either the block
// or the single-line form, and whether or not it is marked indirect.
func declaresRequire(gomod, path string) bool {
	for _, line := range strings.Split(gomod, "\n") {
		f := strings.Fields(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "require ")))
		if len(f) >= 2 && f[0] == path {
			return true
		}
	}
	return false
}

// replaceTarget returns the directory a replace directive points at.
func replaceTarget(gomod, path string) (string, bool) {
	for _, line := range strings.Split(gomod, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "replace ")
		lhs, rhs, ok := strings.Cut(line, "=>")
		if !ok {
			continue
		}
		f := strings.Fields(lhs)
		if len(f) == 0 || f[0] != path {
			continue
		}
		target := strings.TrimSpace(rhs)
		// A version on the right-hand side means a module replacement, not a
		// directory one — no local path to check.
		if fields := strings.Fields(target); len(fields) > 1 {
			return "", false
		}
		return target, true
	}
	return "", false
}

// repoRootFromHere walks up to the directory holding go.work. It FAILS rather
// than skips when it cannot find one: a guard that quietly passes because it
// scanned nothing is worse than no guard.
func repoRootFromHere(t *testing.T) string {
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
	t.Fatalf("no go.work found above %s", dir)
	return ""
}

func relTo(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return rel
	}
	return path
}
