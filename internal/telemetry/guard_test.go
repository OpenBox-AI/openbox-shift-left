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

// The rule it protects is the one that made telemetry/ a separate module at
// all: the collector tree must not leak into gateway/ or decision/, whose own
// guards bound what credential-path code can execute.

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
					t.Errorf("%s: calls %s.%s; this module takes its configuration as a struct "+
						"and emits through the Emitter seam; environment and filesystem access "+
						"belong in the CLI, where credential handling is already scanned",
						filepath.Join(".", fset.Position(call.Pos()).String()), local[ident.Name], fn)
				}
			}
			return true
		})
	}
}
