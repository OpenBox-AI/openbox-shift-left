package decision

import (
	"os"
	"strings"
	"testing"
)

// allowedDirectRequires is this module's entire direct-dependency surface.
//
// It exists because of ADR-0023. `gateway/` imports this module and carries a
// credential guard, and that guard's go.mod half used to reject every requirement
// outside its own two-entry allowlist — transitive ones included. That was only
// sustainable while this module had no external dependencies, which made
// gateway's promise transitive by accident rather than by design.
//
// ADR-0023 narrowed gateway's check to DIRECT requires and moved the bound down
// here: whatever this module takes on is reviewed at this module.
//
// Sibling modules are listed explicitly rather than pattern-excluded, so the list
// reads as the module's whole surface, matching how gateway's allowlist is written.
//
// **The gitleaks entry was added deliberately, with the cost measured first.** This
// is the visible act ADR-0023 exists to force, so the number belongs beside it:
// adopting it grows the CLI binary 8,528,818 -> 11,258,962 bytes (+32%) and this
// module's reachable package set 200 -> 379, linking viper, afero, fsnotify,
// mholt/archives, lipgloss, termenv and zerolog. viper is reachable only because
// NewDetectorDefaultConfig uses it to unmarshal a static embedded TOML string. The
// owner accepted that with the measurement in hand (D-OSS-4).
//
// What it bought: 222 maintained rules replacing nine hand-rolled regexes, plus
// coverage of formats we had no rule for at all. What it did NOT buy: anything on
// the assignment-shaped or unlabelled-high-entropy axes — those remain ours in
// secrets.go, because gitleaks' entropy is a per-rule threshold on a regex match
// rather than a standalone scan.
var allowedDirectRequires = map[string]bool{
	// ADR-0003: the decision module depends on client, never the reverse.
	"github.com/openbox-ai/openbox-shift-left/client": true,
	// D-OSS-4: the named-format detection rule pack.
	"github.com/zricethezav/gitleaks/v8": true,
}

// The module's direct dependencies must be exactly the reviewed set.
//
// This is the load-bearing half of ADR-0023's compensating control. If it is ever
// green for the wrong reason — a parser that matches nothing, an allowlist that
// grew silently — then gateway's narrowed guard is bounding nothing and the ADR's
// argument has quietly stopped holding.
func TestDirectDependenciesAreReviewed(t *testing.T) {
	for _, path := range unallowedDirectRequires(readGoMod(t), allowedDirectRequires) {
		t.Errorf("decision/go.mod directly requires %q, which allowedDirectRequires does not "+
			"name. gateway's credential guard bounds only its own DIRECT requires "+
			"(ADR-0023) and delegates this module's surface here, so a dependency added "+
			"without review is exactly what this catches.", path)
	}
}

// The seeded control: a guard that cannot fail is not a guard.
//
// Fixture bodies rather than the live file, because the live file passing tells
// you nothing about what the check would REJECT — and with a one-entry allowlist
// it passes easily, which is precisely the state where a broken parser looks
// healthy.
func TestGuardRejectsAnUnreviewedDirectRequire(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mod     string
		wantBad []string
	}{
		{
			name: "a direct external require is REJECTED",
			// NOT gitleaks: that is allowlisted now, so using it here would make
			// this case pass for the wrong reason.
			mod: `module github.com/openbox-ai/openbox-shift-left/decision

require github.com/some/unreviewed v1.2.3
`,
			wantBad: []string{"github.com/some/unreviewed"},
		},
		{
			name: "a direct non-github require is REJECTED too",
			mod: `module github.com/openbox-ai/openbox-shift-left/decision

require golang.org/x/text v0.14.0
`,
			wantBad: []string{"golang.org/x/text"},
		},
		{
			name: "an INDIRECT require is accepted — ADR-0023's boundary",
			mod: `module github.com/openbox-ai/openbox-shift-left/decision

require github.com/some/transitive v1.2.3 // indirect
`,
		},
		{
			name: "the allowlisted sibling is accepted",
			mod: `module github.com/openbox-ai/openbox-shift-left/decision

require github.com/openbox-ai/openbox-shift-left/client v0.0.0

replace github.com/openbox-ai/openbox-shift-left/client => ../client
`,
		},
		{
			name: "an unlisted sibling is still REJECTED — being in-repo is not a pass",
			mod: `module github.com/openbox-ai/openbox-shift-left/decision

require github.com/openbox-ai/openbox-shift-left/gateway v0.0.0
`,
			wantBad: []string{"github.com/openbox-ai/openbox-shift-left/gateway"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := unallowedDirectRequires(tc.mod, allowedDirectRequires)
			if len(got) != len(tc.wantBad) {
				t.Fatalf("got %v, want %v", got, tc.wantBad)
			}
			for i, want := range tc.wantBad {
				if got[i] != want {
					t.Errorf("[%d] = %q, want %q", i, got[i], want)
				}
			}
		})
	}
}

// readGoMod reads this module's go.mod.
func readGoMod(t *testing.T) string {
	t.Helper()
	mod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	return string(mod)
}

// unallowedDirectRequires returns the DIRECT module requirements in a go.mod body
// that the allowlist does not name.
//
// Deliberately a copy of gateway's function rather than a shared helper: this is a
// TEST-ONLY guard in a module that must not grow dependencies, and importing it
// from `gateway` would invert the dependency direction (ADR-0003: decision depends
// on client, never the reverse; gateway depends on decision). The two copies are
// kept honest by carrying the same seeded cases rather than by sharing code.
//
// See docs/adr/ADR-0023-credential-guard-scope.md for why indirect requires are
// skipped, and why that is a reduction rather than a clarification.
func unallowedDirectRequires(gomod string, allowed map[string]bool) []string {
	var bad []string
	for _, line := range strings.Split(gomod, "\n") {
		line = strings.TrimSpace(line)

		// `module` names this module; `replace` redirects one, it does not add one.
		if strings.HasPrefix(line, "module ") || strings.HasPrefix(line, "replace ") {
			continue
		}
		if strings.Contains(line, "// indirect") {
			continue
		}
		line = strings.TrimPrefix(line, "require ")
		fields := strings.Fields(line)
		// A requirement is `path version`; anything else is punctuation, a
		// directive, or a comment.
		if len(fields) < 2 || !strings.HasPrefix(fields[1], "v") {
			continue
		}
		path := fields[0]
		first := path
		if i := strings.Index(first, "/"); i >= 0 {
			first = first[:i]
		}
		if !strings.Contains(first, ".") {
			continue
		}
		if !allowed[path] {
			bad = append(bad, path)
		}
	}
	return bad
}
