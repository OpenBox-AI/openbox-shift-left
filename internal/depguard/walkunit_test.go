package depguard

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The walker replaces five guards, so its failure modes are the phase's failure
// modes. Every case below is one of them, not a coverage exercise.

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestSubtreeImports_ClassifiesThreeWays(t *testing.T) {
	root := tree(t, map[string]string{"a.go": `package a
import (
	"fmt"
	"net/http"
	"github.com/example/dep"
	"` + repoPrefix + `/client"
	"` + repoPrefix + `/decision/sub"
	"` + repoPrefix + `/gateway/internal/dialhook"
)
`})
	got, err := subtreeImports(root, repoPrefix+"/gateway")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"github.com/example/dep"}; !reflect.DeepEqual(got.external, want) {
		t.Errorf("external: got %v, want %v", got.external, want)
	}
	want := []string{repoPrefix + "/client", repoPrefix + "/decision/sub"}
	if !reflect.DeepEqual(got.repoLocal, want) {
		t.Errorf("repoLocal: got %v, want %v (the self-subtree import must be dropped)", got.repoLocal, want)
	}
}

// The regression this classification exists to stop. Without the repoLocal half,
// a subtree could import the package that reads ~/.openbox/.env and every guard
// would stay green.
func TestSubtreeImports_RepoLocalIsNotSilentlyDropped(t *testing.T) {
	root := tree(t, map[string]string{
		"a.go": "package a\nimport \"" + repoPrefix + "/internal/cli/devinit\"\n",
	})
	got, _ := subtreeImports(root, repoPrefix+"/gateway")
	if len(got.repoLocal) != 1 {
		t.Fatalf("repoLocal = %v; a repo-local import must be VISIBLE to the guard", got.repoLocal)
	}
}

// Six _GOOS.go files ship. A constraint-evaluating walk is blind to the other
// platform, which is exactly where cross-compilation puts a hole.
func TestSubtreeImports_SeesBuildTaggedFiles(t *testing.T) {
	root := tree(t, map[string]string{
		"w_windows.go": "//go:build windows\n\npackage a\nimport \"github.com/win/only\"\n",
		"u_unix.go":    "//go:build !windows\n\npackage a\nimport \"github.com/unix/only\"\n",
	})
	got, _ := subtreeImports(root, "none")
	if want := []string{"github.com/unix/only", "github.com/win/only"}; !reflect.DeepEqual(got.external, want) {
		t.Errorf("got %v, want %v", got.external, want)
	}
}

// gateway's go.mod half was the only thing bounding its test-only deps. Deleting
// that check is a dissolution rather than a loss only if this walk covers them.
func TestSubtreeImports_IncludesTestFiles(t *testing.T) {
	root := tree(t, map[string]string{
		"a.go":      "package a\n",
		"a_test.go": "package a\nimport \"github.com/test/only\"\n",
	})
	got, _ := subtreeImports(root, "none")
	if want := []string{"github.com/test/only"}; !reflect.DeepEqual(got.external, want) {
		t.Errorf("got %v, want %v", got.external, want)
	}
}

func TestSubtreeImports_AliasedBlankAndDotImportsCount(t *testing.T) {
	root := tree(t, map[string]string{
		"a.go": "package a\nimport (\n\taliased \"github.com/a/one\"\n\t_ \"github.com/b/two\"\n\t. \"github.com/c/three\"\n)\n",
	})
	got, _ := subtreeImports(root, "none")
	want := []string{"github.com/a/one", "github.com/b/two", "github.com/c/three"}
	if !reflect.DeepEqual(got.external, want) {
		t.Errorf("got %v, want %v", got.external, want)
	}
}

func TestSubtreeImports_WalksSubdirectories(t *testing.T) {
	root := tree(t, map[string]string{
		"a.go":       "package a\n",
		"sub/b.go":   "package b\nimport \"github.com/deep/dep\"\n",
		"sub/c/d.go": "package c\nimport \"github.com/deeper/dep\"\n",
	})
	got, _ := subtreeImports(root, "none")
	want := []string{"github.com/deep/dep", "github.com/deeper/dep"}
	if !reflect.DeepEqual(got.external, want) {
		t.Errorf("got %v, want %v — a one-level glob stops covering the subtree the day it grows a package", got.external, want)
	}
}

func TestSubtreeImports_EmptyOrMissingTreeIsAnError(t *testing.T) {
	if _, err := subtreeImports(tree(t, map[string]string{"README.md": "x"}), "none"); err == nil {
		t.Error("an empty subtree must be an error, not a result that satisfies every allowlist")
	}
	if _, err := subtreeImports(filepath.Join(t.TempDir(), "nope"), "none"); err == nil {
		t.Error("a missing root must be an error")
	}
}

// Go's own stdlib rule: no dot in the first path segment.
func TestSubtreeImports_DotInFirstSegmentIsTheStdlibRule(t *testing.T) {
	root := tree(t, map[string]string{
		"a.go": "package a\nimport (\n\t\"internal/thing\"\n\t\"example.com/x\"\n)\n",
	})
	got, _ := subtreeImports(root, "none")
	if want := []string{"example.com/x"}; !reflect.DeepEqual(got.external, want) {
		t.Errorf("got %v, want %v", got.external, want)
	}
}

// Prefix membership: an entry admits its own subpackages, and stops at the slash.
func TestUnallowed_PrefixSemanticsAndItsBoundary(t *testing.T) {
	allow := map[string]bool{"go.opentelemetry.io/collector/pdata": true, repoPrefix + "/gateway": true}
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"module entry admits its subpackages", []string{"go.opentelemetry.io/collector/pdata/plog"}, nil},
		{"module entry admits itself", []string{"go.opentelemetry.io/collector/pdata"}, nil},
		{"a sibling module is NOT admitted", []string{"go.opentelemetry.io/collector/consumer"},
			[]string{"go.opentelemetry.io/collector/consumer"}},
		{"the slash boundary holds: gatewayfoo is not gateway", []string{repoPrefix + "/gatewayfoo"},
			[]string{repoPrefix + "/gatewayfoo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unallowed(tc.in, allow); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDead_ReportsAnEntryNothingImports(t *testing.T) {
	allow := map[string]bool{"github.com/used/x": true, "github.com/unused/y": true}
	got := dead([]string{"github.com/used/x/sub"}, allow)
	if want := []string{"github.com/unused/y"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
