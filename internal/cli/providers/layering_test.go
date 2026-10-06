package providers_test

import (
	"go/build"
	"strings"
	"testing"
)

// TestOnlyTheRegistryImportsAdapters the CLI must reach adapters only through
// this registry.
func TestOnlyTheRegistryImportsAdapters(t *testing.T) {
	for _, pkg := range []string{
		"github.com/openbox-ai/openbox-shift-left/cmd/openbox",
		"github.com/openbox-ai/openbox-shift-left/internal/cli/devinit",
		"github.com/openbox-ai/openbox-shift-left/internal/cli/backend",
		"github.com/openbox-ai/openbox-shift-left/internal/cli/managed",
	} {
		p, err := build.Import(pkg, "", build.FindOnly|build.ImportComment)
		if err != nil {
			t.Fatalf("cannot resolve %s: %v", pkg, err)
		}
		full, err := build.ImportDir(p.Dir, 0)
		if err != nil {
			t.Fatalf("import %s: %v", pkg, err)
		}
		for _, imp := range full.Imports {
			if strings.Contains(imp, "/internal/adapters/claude-code") || strings.Contains(imp, "/internal/adapters/codex") {
				t.Errorf("%s imports the adapter %q directly; route it through internal/cli/providers, "+
					"or use the shared devconfig/hookflow package if the need is provider-neutral", pkg, imp)
			}
		}
	}
}
