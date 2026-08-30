package gateway

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenCalls are the ways this module could start resolving a credential,
// keyed by IMPORT PATH rather than by the identifier at the call site. Keying on
// the identifier is what an alias defeats: `import os2 "os"` then `os2.Getenv(…)`
// presents a selector whose package name matches nothing.
//
// syscall and io/ioutil are here because they are the same capability under
// other names — syscall.Environ() takes no arguments and returns the whole
// environment, so a guard watching only os.Getenv would not see it.
var forbiddenCalls = map[string][]string{
	"os":        {"Getenv", "LookupEnv", "Environ", "ReadFile", "Open", "OpenFile", "ReadDir", "UserHomeDir", "UserConfigDir"},
	"syscall":   {"Getenv", "Environ", "Open", "Read"},
	"io/ioutil": {"ReadFile", "ReadDir"},
}

// forbiddenLiterals are credential coordinates. Matching is case-insensitive:
// the same env var spelled in lower case is the same read. This is the weaker of
// the two checks — it can only catch names it already knows — which is why the
// call check is keyed on resolved imports rather than leaning on this list.
var forbiddenLiterals = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"OPENAI_API_KEY",
	"OPENBOX_AGENT_PRIVATE_KEY",
	"OPENBOX_ED25519_SEED",
	".openbox",
	"api_key",
}

// guardHit is one reason a source file failed the guard.
type guardHit struct {
	pos  string
	what string
}

// scanSource is the ONE implementation of the credential-read check.
//
// Both the guard and its mutation control call this, deliberately: a control
// that re-implemented the walk would keep validating the old logic after the
// real check was strengthened, and go on reporting green. That is the same drift
// this repo already fixed by building `doctor`'s duplicate-hook warning and
// `init`'s repair on one shared classifier, so the check and the fix cannot
// disagree about what they cover.
func scanSource(fset *token.FileSet, file *ast.File) []guardHit {
	var hits []guardHit
	at := func(n ast.Node) string { return fset.Position(n.Pos()).String() }

	// Resolve the identifier each import is actually used under, so an alias is
	// followed rather than missed.
	bindings := map[string]string{}
	for _, spec := range file.Imports {
		importPath := strings.Trim(spec.Path.Value, `"`)
		name := importPath
		if idx := strings.LastIndex(name, "/"); idx >= 0 {
			name = name[idx+1:]
		}
		if spec.Name != nil {
			switch spec.Name.Name {
			case ".":
				// A dot-import erases the qualifier, so no selector-based check
				// can see through it. Refuse the construct instead of trying.
				hits = append(hits, guardHit{at(spec), fmt.Sprintf("dot-import of %q erases the package qualifier", importPath)})
				continue
			case "_":
				continue
			default:
				name = spec.Name.Name
			}
		}
		bindings[name] = importPath
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			qualifier, ok := node.X.(*ast.Ident)
			if !ok {
				return true
			}
			importPath, isImport := bindings[qualifier.Name]
			if !isImport {
				return true
			}
			for _, banned := range forbiddenCalls[importPath] {
				if node.Sel.Name == banned {
					hits = append(hits, guardHit{at(node), fmt.Sprintf("%s.%s (%s) is a credential-capable read", qualifier.Name, banned, importPath)})
				}
			}
		case *ast.BasicLit:
			if node.Kind != token.STRING {
				return true
			}
			lowered := strings.ToLower(node.Value)
			for _, banned := range forbiddenLiterals {
				if strings.Contains(lowered, strings.ToLower(banned)) {
					hits = append(hits, guardHit{at(node), fmt.Sprintf("source mentions credential coordinate %q", banned)})
				}
			}
		}
		return true
	})
	return hits
}

// moduleSources returns every non-test Go file in this module, walking
// subdirectories rather than globbing one level. The module is flat today; a
// guard that assumed it stays flat would silently stop covering the module the
// first time it grew an internal package.
func moduleSources(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walking module sources: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no non-test sources found; the guard would pass vacuously")
	}
	return out
}

// TestGatewayReadsNoCredential is the guard that keeps auth handling from
// creeping back in, in the same spirit as the registry-import guard in cli/.
// Requirement: the module contains no credential resolution path — no env read,
// no file read.
//
// One stdlib exception is deliberate and named here so it is not mistaken for an
// oversight: http.ProxyFromEnvironment reads HTTP_PROXY/HTTPS_PROXY/NO_PROXY
// inside net/http. Those are the developer's own proxy coordinates, not a
// provider credential, and honouring them is what lets the relay work behind a
// corporate proxy.
//
// Scope: this scans this SUBTREE's own non-test files, and the import allowlist
// is what makes that sufficient rather than lucky — with no external import
// reachable, there is no third-party package for a credential read to hide in.
//
// That allowlist lives in internal/depguard now, with the other four. It used to
// be duplicated here, and the two copies had already drifted: this file's
// moduleSources skips _test.go (correctly — the fixtures below contain credential
// literals on purpose), so the local copy could not see an unreviewed import
// added in a test file, while depguard's walk can. Two allowlists for one subtree,
// one of them strictly weaker, is the shape this repo already paid for once.
func TestGatewayReadsNoCredential(t *testing.T) {
	for _, path := range moduleSources(t) {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, hit := range scanSource(fset, file) {
			t.Errorf("%s: %s; the gateway must resolve no credentials", hit.pos, hit.what)
		}
	}

	// The guard is only evidence if it is actually looking at the relay.
	if _, err := os.Stat("proxy.go"); err != nil {
		t.Fatalf("proxy.go not found, so the guard is not covering the relay: %v", err)
	}
}

// TestGuardCatchesACredentialRead is the mutation control: a guard that cannot
// fail is not a guard. It drives scanSource — the same function the guard uses —
// with source that does read a credential, including the evasions a name-keyed
// or case-sensitive check would miss.
func TestGuardCatchesACredentialRead(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"plain os.Getenv", `package gateway
import "os"
func leak() string { return os.Getenv("ANTHROPIC_API_KEY") }
`},
		{"aliased os import, no literal to fall back on", `package gateway
import stdos "os"
func leak() []string { return stdos.Environ() }
`},
		{"aliased os with runtime-assembled name", `package gateway
import stdos "os"
func leak(prefix, suffix string) (string, bool) { return stdos.LookupEnv(prefix + suffix) }
`},
		{"syscall.Environ", `package gateway
import "syscall"
func leak() []string { return syscall.Environ() }
`},
		{"syscall.Getenv", `package gateway
import "syscall"
func leak() (string, bool) { return syscall.Getenv("anything") }
`},
		{"ioutil.ReadFile", `package gateway
import "io/ioutil"
func leak() ([]byte, error) { return ioutil.ReadFile("/home/dev/.aws/credentials") }
`},
		{"lowercase credential literal", `package gateway
func leak() string { return "anthropic_api_key" }
`},
		{"dot-import hides the qualifier", `package gateway
import . "os"
func leak() string { return Getenv("SOMETHING") }
`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "offending.go")
			if err := os.WriteFile(path, []byte(tc.src), 0o600); err != nil {
				t.Fatalf("writing fixture: %v", err)
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parsing fixture: %v", err)
			}
			if hits := scanSource(fset, file); len(hits) == 0 {
				t.Errorf("scanSource did not flag %s; the guard can be evaded that way", tc.name)
			}
		})
	}
}

// TestGuardPassesCleanSource is the other half of the control: a scanner that
// flagged everything would also "catch" every evasion while being useless.
func TestGuardPassesCleanSource(t *testing.T) {
	const clean = `package gateway
import (
	"net/http"
	"strings"
)
func fine(h http.Header) string { return strings.ToLower(h.Get("Anthropic-Version")) }
`
	dir := t.TempDir()
	path := filepath.Join(dir, "clean.go")
	if err := os.WriteFile(path, []byte(clean), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	if hits := scanSource(fset, file); len(hits) != 0 {
		t.Errorf("scanSource flagged clean source: %v", hits)
	}
}
