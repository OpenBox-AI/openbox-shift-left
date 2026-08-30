package providers_test

import (
	"go/build"
	"strings"
	"testing"
)

// The CLI must reach adapters only through this registry. Provider-neutral work
// — resolving config, the policy bundle path, credentials — belongs to the
// shared modules, but the command wiring used to call claude-code's aliases for
// all of it, so a Codex-only user's `dev sync` ran through the Claude Code
// package for no reason.
//
// This pins the direction: the composition root imports adapters, and nothing
// else in cli/ does.
func TestOnlyTheRegistryImportsAdapters(t *testing.T) {
	for _, pkg := range []string{
		"github.com/openbox-ai/openbox-shift-left/cmd/openbox",
		"github.com/openbox-ai/openbox-shift-left/internal/cli/devinit",
		"github.com/openbox-ai/openbox-shift-left/internal/cli/backend",
		"github.com/openbox-ai/openbox-shift-left/internal/cli/managed",
	} {
		p, err := build.Import(pkg, "", build.FindOnly|build.ImportComment)
		if err != nil {
			// Not a skip. A guard that quietly passes because it resolved nothing
			// reports the same thing as a guard that found nothing wrong, and this
			// repo has already been bitten by exactly that.
			t.Fatalf("cannot resolve %s: %v", pkg, err)
		}
		full, err := build.ImportDir(p.Dir, 0)
		if err != nil {
			t.Fatalf("import %s: %v", pkg, err)
		}
		for _, imp := range full.Imports {
			if strings.Contains(imp, "/adapters/claude-code") || strings.Contains(imp, "/adapters/codex") {
				t.Errorf("%s imports the adapter %q directly — route it through cli/internal/providers, "+
					"or use the shared devconfig/hookflow package if the need is provider-neutral", pkg, imp)
			}
		}
	}
}
