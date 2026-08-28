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

// allowedDirectRequires is every module this one may require directly.
//
// All eight are the collector's own family, and that is the point: adding
// otlpreceiver was one decision, not eight. A require from any other host — or a
// second family — is a new decision and must fail here first.
//
// Note what is ABSENT and was expected: `client` and `decision`. The plan
// budgeted for both, and the Emitter interface removed the need — this module
// hands out a normalized Record and never builds an event, signs a payload, or
// runs the redactor. Keeping it that way is worth more than the convenience of
// importing them, because it is what lets this tree stay quarantined.
var allowedDirectRequires = map[string]bool{
	"go.opentelemetry.io/collector/component":             true,
	"go.opentelemetry.io/collector/config/configgrpc":     true,
	"go.opentelemetry.io/collector/config/confighttp":     true,
	"go.opentelemetry.io/collector/config/configoptional": true,
	"go.opentelemetry.io/collector/consumer":              true,
	"go.opentelemetry.io/collector/pdata":                 true,
	"go.opentelemetry.io/collector/receiver":              true,
	"go.opentelemetry.io/collector/receiver/otlpreceiver": true,
}

// TestOnlyReviewedDirectRequires reads go.mod and fails on any direct require
// outside the allowlist.
//
// Matching is host-agnostic — every non-comment require line is checked, not only
// those starting `github.com/`. The gateway's original guard matched that one
// prefix, which made a direct `golang.org/x/…` or `go.opentelemetry.io/…` require
// invisible to it; this module is built almost entirely from the latter, so that
// bug would have made the guard vacuous here.
func TestOnlyReviewedDirectRequires(t *testing.T) {
	raw, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, path := range directRequires(string(raw)) {
		if !allowedDirectRequires[path] {
			t.Errorf("go.mod takes a direct dependency on %q, which is outside this module's "+
				"reviewed set. Adding one is a decision: record it, and check it does not "+
				"belong behind the Emitter seam instead.", path)
		}
	}
}

// TestAllowlistHasNoDeadEntries keeps the list honest in the other direction.
// An allowlist that names modules the go.mod no longer requires reads as broader
// review than actually happened.
func TestAllowlistHasNoDeadEntries(t *testing.T) {
	raw, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	present := map[string]bool{}
	for _, p := range directRequires(string(raw)) {
		present[p] = true
	}
	for allowed := range allowedDirectRequires {
		if !present[allowed] {
			t.Errorf("allowlist names %q but go.mod no longer requires it directly; "+
				"drop it rather than leaving a claim of review standing", allowed)
		}
	}
}

// directRequires returns the module paths of every non-indirect require.
func directRequires(mod string) []string {
	var out []string
	inBlock := false
	for _, line := range strings.Split(mod, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "//"), trimmed == "":
			continue
		case trimmed == "require (":
			inBlock = true
			continue
		case inBlock && trimmed == ")":
			inBlock = false
			continue
		}
		if strings.Contains(trimmed, "// indirect") {
			continue
		}
		fields := strings.Fields(trimmed)
		switch {
		case inBlock && len(fields) >= 2:
			out = append(out, fields[0])
		case !inBlock && len(fields) >= 3 && fields[0] == "require":
			// The single-line form: `require path v1.2.3`.
			out = append(out, fields[1])
		}
	}
	return out
}

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
