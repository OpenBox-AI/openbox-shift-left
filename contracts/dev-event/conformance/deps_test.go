package conformance

import (
	"os"
	"strings"
	"testing"
)

// allowedDependencies is this module's entire non-stdlib surface.
//
// It started EMPTY, and the change from "zero dependencies" to "these two" is
// worth stating rather than absorbing. The module shipped a 211-line hand-rolled
// draft-2020-12 subset so that `go test ./...` ran offline with nothing to
// download, and so the three adapters that import this package in their tests
// pulled nothing in. Both were real benefits and both are now partly spent
// (D-OSS-5).
//
// What was bought: the whole draft instead of fourteen keywords. The retired walk
// resolved only local `#/$defs/` refs, honoured `additionalProperties` in its
// boolean form alone, and silently ignored every keyword outside its subset — so
// a constraint written with an unimplemented keyword read as a tightened contract
// and enforced nothing. That is a live risk rather than a theoretical one:
// contract v1.6 adds oneOf discriminator branches the walk had never been
// stressed on.
//
// What it costs: golang.org/x/text is a genuine (non-test) requirement of the
// validator, so it joins the test dependency graph of adapters/claude-code,
// adapters/codex and client too — the accepted cost recorded in the plan's
// dependency story, not an oversight.
//
// The list stays SHORT deliberately. A convenience import here spreads to three
// other modules, so adding to it is a decision, which is the point of making it
// fail here first.
var allowedDependencies = map[string]bool{
	"github.com/santhosh-tekuri/jsonschema/v6": true,
	"golang.org/x/text":                        true,
}

// The module's dependencies must be exactly the reviewed set.
//
// This replaces TestModuleStaysDependencyFree, which asserted go.mod carried no
// `require` at all. That assertion was correct until the validator swap and would
// now fail permanently; an allowlist keeps the same protective intent — nothing
// arrives unreviewed — against the new baseline. go.mod is the whole gate: a Go
// module cannot import what it does not require.
func TestDependenciesAreOnTheAllowlist(t *testing.T) {
	raw, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)

		// A `replace` would make this contract module resolve differently per
		// checkout, so it stays forbidden outright rather than allowlisted.
		if strings.HasPrefix(line, "replace ") {
			t.Errorf("conformance gained a replace directive (%q). The contract module must "+
				"resolve identically in every checkout.", line)
			continue
		}

		path, ok := requiredModule(line)
		if !ok {
			continue
		}
		if !allowedDependencies[path] {
			t.Errorf("conformance requires %q, which is not on allowedDependencies. Three "+
				"adapters import this package in their tests, so a dependency here spreads to "+
				"their graphs too — add it deliberately, or move whatever needs it to a caller.", path)
		}
	}
}

// requiredModule extracts the module path from a go.mod requirement line, in
// either the single-line (`require x v1`) or block (`x v1`) form.
func requiredModule(line string) (string, bool) {
	fields := strings.Fields(strings.TrimPrefix(line, "require "))
	if len(fields) < 2 || !strings.HasPrefix(fields[1], "v") {
		return "", false
	}
	// A stdlib path's first segment carries no dot; anything module-ish does.
	first := fields[0]
	if i := strings.Index(first, "/"); i >= 0 {
		first = first[:i]
	}
	if !strings.Contains(first, ".") {
		return "", false
	}
	return fields[0], true
}
