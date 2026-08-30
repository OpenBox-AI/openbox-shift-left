package depguard

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The five subtree dependency guards, replacing the go.mod-reading tests in
// decision, telemetry, transport, gateway and conformance.
//
// Each allowlist below is TODAY'S list, unchanged. Entries are MODULE paths and
// admit their own subpackages, which is what a `require` already meant --
// telemetry allows `collector/pdata` and imports `pdata/plog`; gateway allows
// `.../client` and imports `client/memhttptest`. Nothing was added to make a
// list pass; if one ever needs to be, that is an ADR-0023 amendment and not a
// commit.
//
// Two directions per guard, because two of the originals carry both:
// nothing unreviewed enters, and no reviewed entry is stale. An entry nobody
// imports is a standing claim of review with nothing behind it.
//
// NO PATH HERE IS IN A FORM AUTOMATION CAN FIND.
//
// Subtree names are bare (`"decision"`), allowlist entries are built by
// concatenation (`repoPrefix + "/client"`), and roots are assembled with
// filepath.Join -- so none of them ever contains the contiguous qualified import
// path that a mechanical rewrite matches on. The module collapse had to update
// this file by hand for exactly that reason, and the next move will too.
//
// The failure mode is loud but misleading: a stale path makes the unallowed and
// dead-entry tests fire together, which reads as "the guards broke" rather than
// "a path went stale". If you move a subtree, change it here and re-run the
// drills -- the one that matters is internal/gateway importing
// internal/cli/devinit, the package that reads ~/.openbox/.env.

type subtreeGuard struct {
	name string // the subtree, repo-relative
	// external is every non-repo module the subtree may import.
	external map[string]bool
	// repoLocal is every OTHER subtree of this repo it may reach.
	//
	// This half is not decoration. gateway's entire allowlist is repo-local and
	// telemetry's is deliberately EMPTY -- the quarantine its own guard calls out
	// ("note what is ABSENT and was expected"). Drop this axis and
	// `internal/gateway` could import `internal/cli/devinit`, which reads and
	// writes ~/.openbox/.env, with every guard still green.
	repoLocal map[string]bool
	// why documents anything a reader would otherwise mistake for an oversight.
	why string
}

func guards() []subtreeGuard {
	return []subtreeGuard{
		{
			name: "internal/decision",
			external: map[string]bool{
				// D-OSS-4: the named-format detection rule pack.
				"github.com/zricethezav/gitleaks/v8": true,
			},
			repoLocal: map[string]bool{
				// ADR-0003: the decision module depends on client, never the reverse.
				repoPrefix + "/internal/client": true,
			},
			why: "the load-bearing half of ADR-0023's compensating control",
		},
		{
			name: "internal/telemetry",
			external: map[string]bool{
				"go.opentelemetry.io/collector/component":             true,
				"go.opentelemetry.io/collector/config/configgrpc":     true,
				"go.opentelemetry.io/collector/config/confighttp":     true,
				"go.opentelemetry.io/collector/config/configoptional": true,
				"go.opentelemetry.io/collector/consumer":              true,
				"go.opentelemetry.io/collector/pdata":                 true,
				"go.opentelemetry.io/collector/receiver":              true,
				"go.opentelemetry.io/collector/receiver/otlpreceiver": true,
				// component.TelemetrySettings types its fields as *zap.Logger,
				// trace.TracerProvider and metric.MeterProvider, so a non-nil value
				// cannot be supplied without naming these. The zero value crashed
				// the receiver on its first real start.
				"go.uber.org/zap":                 true,
				"go.opentelemetry.io/otel/trace":  true,
				"go.opentelemetry.io/otel/metric": true,
			},
			repoLocal: map[string]bool{},
			why: "the empty repoLocal set IS the control: this lane reaches neither " +
				"client nor decision, and an entry appearing here is the quarantine breaking",
		},
		{
			name:     "internal/transport",
			external: map[string]bool{"github.com/elazarl/goproxy": true},
			repoLocal: map[string]bool{
				// gateway, because this lane REUSES the relay rather than forking
				// it. Serving the existing gateway.Gateway over the hijacked
				// connection is why nothing here imports client or decision --
				// the credential-path surface is SMALLER than phase 11 planned.
				repoPrefix + "/internal/gateway": true,
			},
			why: "phase 11 expected {goproxy, gateway, client, decision}; the reuse decision removed two",
		},
		{
			name: "internal/gateway",
			// Empty is the strongest statement available here, not a vacuous one:
			// the relay imports NO external code at all, which is what makes the
			// lexical credential scan in gateway/guard_test.go sufficient rather
			// than lucky -- there is no third-party package for a credential read
			// to hide in.
			external: map[string]bool{},
			repoLocal: map[string]bool{
				repoPrefix + "/internal/client":   true,
				repoPrefix + "/internal/decision": true,
			},
			why: "ADR-0023's subject; the allowlist bounds what the lexical scan cannot follow",
		},
	}
}

func TestSubtreeDependenciesAreReviewed(t *testing.T) {
	root := repoRoot(t)
	for _, g := range guards() {
		t.Run(g.name, func(t *testing.T) {
			self := repoPrefix + "/" + g.name
			got, err := subtreeImports(filepath.Join(root, g.name), self)
			if err != nil {
				t.Fatalf("%s: %v", g.name, err)
			}
			for _, p := range unallowed(got.external, g.external) {
				t.Errorf("%s imports external %q, which its allowlist does not name (%s). "+
					"Add it deliberately -- widening a list to make an import pass is what "+
					"ADR-0023 forbids -- or move whatever needs it to a caller.", g.name, p, g.why)
			}
			for _, p := range unallowed(got.repoLocal, g.repoLocal) {
				t.Errorf("%s imports repo-local %q, which its allowlist does not name. "+
					"Under one module the compiler permits this; this list is the only "+
					"thing that does not.", g.name, p)
			}
		})
	}
}

func TestSubtreeAllowlistsHaveNoDeadEntries(t *testing.T) {
	root := repoRoot(t)
	for _, g := range guards() {
		t.Run(g.name, func(t *testing.T) {
			self := repoPrefix + "/" + g.name
			got, err := subtreeImports(filepath.Join(root, g.name), self)
			if err != nil {
				t.Fatalf("%s: %v", g.name, err)
			}
			for _, p := range dead(got.external, g.external) {
				t.Errorf("%s allows external %q but imports nothing under it; drop it rather "+
					"than leaving a claim of review standing", g.name, p)
			}
			for _, p := range dead(got.repoLocal, g.repoLocal) {
				t.Errorf("%s allows repo-local %q but imports nothing under it; drop it", g.name, p)
			}
		})
	}
}

// conformanceAllowed is the contract module's entire non-stdlib surface, and it
// is a CLOSURE rather than a direct-import list.
//
// The distinction is the whole guard. `golang.org/x/text` is not imported by any
// file here -- it arrives through jsonschema -- so a direct-import check cannot
// see it and would silently drop the entry. That would not be equivalence, and
// this is the one guard ADR-0023 deliberately left closure-wide: its closure is
// two entries, which is readable, and the reason the bound is tight is that
// three adapters import this package in their TESTS, so anything reaching here
// links into their test binaries too. Link-time spread follows the import graph,
// not go.mod, so collapsing the modules does not relax it.
var conformanceAllowed = map[string]bool{
	"github.com/santhosh-tekuri/jsonschema/v6": true,
	"golang.org/x/text":                        true,
}

// TestConformanceClosureIsReviewed fails hard rather than skipping when `go list`
// cannot answer: if the module cache were unpopulated this test binary could not
// have compiled, so an error here is a real result, not a missing capability.
func TestConformanceClosureIsReviewed(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "internal", "conformance")
	cmd := exec.Command("go", "list", "-deps", "-test",
		"-f", "{{if .Module}}{{.Module.Path}}{{end}}", ".")
	cmd.Dir = dir
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps -test in %s: %v", dir, err)
	}

	seen := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		m := strings.TrimSpace(line)
		if m == "" || under(m, repoPrefix) {
			continue
		}
		seen[m] = true
	}
	if len(seen) == 0 {
		t.Fatal("go list returned no modules; the guard would pass vacuously")
	}

	got := make([]string, 0, len(seen))
	for m := range seen {
		got = append(got, m)
	}
	sort.Strings(got)
	for _, m := range got {
		if !conformanceAllowed[m] {
			t.Errorf("conformance's closure contains %q, which conformanceAllowed does not name. "+
				"Three adapters import this package in their tests, so a dependency here spreads "+
				"to their test binaries too -- add it deliberately, or move whatever needs it to "+
				"a caller.", m)
		}
	}
	for m := range conformanceAllowed {
		if !seen[m] {
			t.Errorf("conformanceAllowed names %q but the closure does not contain it; drop it", m)
		}
	}
}

// TestNoReplacePointsOutsideTheRepo is the half of conformance's old guard that
// has to outlive the phase.
//
// Its point was that the contract module must resolve identically in every
// checkout, and a `replace` is what breaks that. Phase 03's acceptance criterion
// says the collapsed repo carries no replace at all -- but a criterion is checked
// once, at merge, and a test is checked forever. This is the forever half.
//
// It means the same thing on both sides of the collapse: today the 45 intra-repo
// replaces all resolve INSIDE the repository and pass; afterwards there are none
// and it guards the root go.mod instead.
func TestNoReplacePointsOutsideTheRepo(t *testing.T) {
	root := repoRoot(t)
	mods := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "go.mod" {
			return nil
		}
		mods++
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, line := range strings.Split(string(raw), "\n") {
			target, ok := replaceTarget(line)
			if !ok {
				continue
			}
			abs := filepath.Clean(filepath.Join(filepath.Dir(path), target))
			rel, rerr := filepath.Rel(root, abs)
			if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Errorf("%s replaces with %q, which resolves outside the repository -- the build "+
					"would then depend on where the checkout sits on disk", mustRel(root, path), target)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if mods == 0 {
		t.Fatal("no go.mod found; the guard would pass vacuously")
	}
}

// replaceTarget returns the filesystem target of a `replace ... => <path>` line,
// and false for any line that is not a replace onto a local path. A replace onto
// another MODULE (no leading . or /) is a different thing and not this subject.
func replaceTarget(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "//") {
		return "", false
	}
	_, after, found := strings.Cut(line, "=>")
	if !found {
		return "", false
	}
	target := strings.TrimSpace(after)
	if i := strings.Index(target, "//"); i >= 0 {
		target = strings.TrimSpace(target[:i])
	}
	if len(strings.Fields(target)) != 1 {
		return "", false
	}
	if !strings.HasPrefix(target, ".") && !strings.HasPrefix(target, "/") {
		return "", false
	}
	return target, true
}

func mustRel(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return rel
	}
	return path
}

// TestAdaptersDoNotImportEachOther is the rule ADR-0011's reason 2 used to get
// from the compiler.
//
// While the adapters are separate modules, one importing the other needs a
// `require` and a `replace` -- mechanical, and nobody has to remember it. Under
// one module they are siblings under internal/adapters/ and the compiler permits
// it, so this test is the whole control. That is a real downgrade from a compiler
// guarantee to a test, and the ADR records it as one.
//
// A negative rule rather than a positive allowlist, deliberately: neither adapter
// has an allowlist today, and inventing two would be a new control rather than a
// converted one.
func TestAdaptersDoNotImportEachOther(t *testing.T) {
	root := repoRoot(t)
	for _, pair := range []struct{ subject, forbidden string }{
		{"internal/adapters/claude-code", "internal/adapters/codex"},
		{"internal/adapters/codex", "internal/adapters/claude-code"},
	} {
		t.Run(pair.subject, func(t *testing.T) {
			self := repoPrefix + "/" + pair.subject
			got, err := subtreeImports(filepath.Join(root, pair.subject), self)
			if err != nil {
				t.Fatalf("%s: %v", pair.subject, err)
			}
			forbidden := repoPrefix + "/" + pair.forbidden
			for _, p := range got.repoLocal {
				if under(p, forbidden) {
					t.Errorf("%s imports %q. Adapters are peers: shared behaviour belongs in "+
						"adapters/common, and provider selection belongs in the registry.",
						pair.subject, p)
				}
			}
		})
	}
}
