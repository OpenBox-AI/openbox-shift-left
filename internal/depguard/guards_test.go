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

// The five subtree dependency guards. Each allowlist entry is a module path and
// admits that module's subpackages, which is what a `require` already meant;
// nothing is added to make a list pass, and adding one is a decision.
//
// Two directions per guard: nothing unreviewed enters, and no reviewed entry is
// stale, because an entry nobody imports is a claim of review with nothing
// behind it.
//
// No path here is in a form automation can find. Names are bare, entries are
// built by concatenation and roots by filepath.Join, so none contains the
// contiguous import path a mechanical rewrite matches on. Moving a subtree means
// editing this file by hand.

type subtreeGuard struct {
	name string // the subtree, repo-relative
	// external is every non-repo module the subtree may import.
	external map[string]bool
	// repoLocal is every OTHER subtree of this repo it may reach.
	//
	// This half is not decoration. gateway's entire allowlist is repo-local and
	// telemetry's is deliberately empty; the quarantine its own guard calls out
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
				// The named-format detection rule pack.
				"github.com/zricethezav/gitleaks/v8": true,
			},
			repoLocal: map[string]bool{
				// The decision module depends on client, never the reverse.
				repoPrefix + "/internal/client": true,
			},
			why: "decision depends on client, never the reverse, and this list is what holds that direction",
		},
		{
			name: "internal/telemetry",
			external: map[string]bool{
				"go.opentelemetry.io/collector/component":         true,
				"go.opentelemetry.io/collector/config/configgrpc": true,
				"go.opentelemetry.io/collector/config/confighttp": true,
				// Names the path-alias middleware in the HTTP server config, which is
				// how Muse's own export paths reach the one OTLP handler.
				"go.opentelemetry.io/collector/config/configmiddleware": true,
				"go.opentelemetry.io/collector/config/configoptional":   true,
				"go.opentelemetry.io/collector/consumer":                true,
				"go.opentelemetry.io/collector/pdata":                   true,
				"go.opentelemetry.io/collector/receiver":                true,
				"go.opentelemetry.io/collector/receiver/otlpreceiver":   true,
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
				// gateway, because this lane reuses the relay rather than forking
				// it. Serving the existing gateway.Gateway over the hijacked
				// connection is why nothing here imports client or decision, which keeps
				// the credential-path surface small.
				repoPrefix + "/internal/gateway": true,
			},
			why: "reusing the gateway relay keeps client and decision out of this subtree",
		},
		{
			name: "internal/gateway",
			// One reviewed external, and the entry IS the review: a pure
			// decompressor, no I/O, no credential surface, reachable only from the
			// teed capture copy. It was added because `br` is the modal response
			// encoding (roughly nine in ten recorded responses) and a gzip-only
			// decode set stored a marker for every one of them.
			//
			// The empty set this replaces was the strongest statement available at
			// the time, and it is worth being precise about what changed and what
			// did not. What made the lexical credential scan in
			// gateway/guard_test.go sufficient was never that the count was zero;
			// it was that nothing here could reach a credential the scan cannot
			// follow. brotli reads bytes and returns bytes. What the scan still
			// cannot follow is bounded by this list, which is why the deliberate
			// move was a named entry here rather than an internal/bodycodec hop:
			// depguard resolves DIRECT imports only, so a one-consumer indirection
			// would have kept this list lexically empty while hiding the
			// dependency from every guard in the repo.
			external: map[string]bool{"github.com/andybalholm/brotli": true},
			repoLocal: map[string]bool{
				repoPrefix + "/internal/client":   true,
				repoPrefix + "/internal/decision": true,
			},
			why: "the gateway reaches client and decision directly; the allowlist bounds what the lexical credential scan cannot follow",
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
					"this guard forbids -- or move whatever needs it to a caller.", g.name, p, g.why)
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
// file here; it arrives through jsonschema -- so a direct-import check cannot
// see it and would silently drop the entry. That would not be equivalence, and
// this is the one guard deliberately left closure-wide: its
// closure is two entries, which is readable, and the reason the bound is tight
// is that three adapters import this package in their tests, so anything
// reaching here links into their test binaries too. Link-time spread follows the
// import graph, not go.mod, so collapsing the modules does not relax it.
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

// TestNoReplacePointsOutsideTheRepo: the contract module must resolve
// identically in every checkout, and a local `replace` resolving outside the
// repository is what breaks that: the build would depend on where the checkout
// sits on disk. Every go.mod in the tree is checked.
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
// another module (no leading . or /) is a different thing and not this subject.
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

// TestAdaptersDoNotImportEachOther is a rule the compiler would enforce if the
// adapters were separate modules, where one importing the other needs a
// `require` and a `replace`. Under one module they are siblings under
// internal/adapters/ and the compiler permits it, so this test is the whole
// control: a real downgrade from a compiler guarantee to a test.
//
// A negative rule rather than a positive allowlist, deliberately: neither adapter
// has an allowlist today, and inventing two would be a new control rather than a
// converted one.
func TestAdaptersDoNotImportEachOther(t *testing.T) {
	root := repoRoot(t)
	for _, pair := range []struct{ subject, forbidden string }{
		{"internal/adapters/claude-code", "internal/adapters/codex"},
		{"internal/adapters/codex", "internal/adapters/claude-code"},
		{"internal/adapters/claude-code", "internal/adapters/muse"},
		{"internal/adapters/codex", "internal/adapters/muse"},
		{"internal/adapters/muse", "internal/adapters/claude-code"},
		{"internal/adapters/muse", "internal/adapters/codex"},
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
