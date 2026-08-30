package telemetry

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This module links the largest third-party surface in the product, so its
// boundary is enforced rather than remembered.
//
// The rule it protects is the one that made telemetry/ a separate module at all:
// the collector tree must not leak into gateway/ or decision/, whose own guards
// bound what credential-path code can execute. A module boundary is a real
// control only while something checks it — otherwise it is a directory name.
//
// Scope, per ADR-0023: DIRECT requires only. Transitive code is bounded at the
// module that took the dependency. Enumerating the closure here would make the
// allowlist unreadable, which is the one thing it must not be.

// forbiddenCalls are the ways this module could start reading a credential or the
// developer's files, keyed by IMPORT PATH rather than by the identifier at the
// call site — an alias defeats identifier matching (`import os2 "os"`).
//
// This module runs as a daemon with content flowing through it. It has no reason
// to read the environment or the filesystem: its configuration arrives as a
// struct and its output leaves through the Emitter. Anything else is the CLI's
// job, where the credential handling already lives and is already scanned.
var forbiddenCalls = map[string][]string{
	"os":        {"Getenv", "LookupEnv", "Environ", "ReadFile", "Open", "OpenFile", "UserHomeDir", "UserConfigDir"},
	"syscall":   {"Getenv", "Environ", "Open", "Read"},
	"io/ioutil": {"ReadFile", "ReadDir"},
}

// TestNoCredentialOrFileReads parses every non-test file and fails on a
// forbidden call.
func TestNoCredentialOrFileReads(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		// Resolve each import's local name to its path, so an alias is followed
		// rather than trusted.
		local := map[string]string{}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			alias := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				alias = imp.Name.Name
			}
			local[alias] = path
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			for _, fn := range forbiddenCalls[local[ident.Name]] {
				if sel.Sel.Name == fn {
					t.Errorf("%s: calls %s.%s — this module takes its configuration as a struct "+
						"and emits through the Emitter seam; environment and filesystem access "+
						"belong in the CLI, where credential handling is already scanned",
						filepath.Join(".", fset.Position(call.Pos()).String()), local[ident.Name], fn)
				}
			}
			return true
		})
	}
}
