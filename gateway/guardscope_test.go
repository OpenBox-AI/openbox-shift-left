package gateway

import (
	"strings"
	"testing"
)

// The go.mod half of the credential guard, exercised against fixture go.mod
// bodies rather than the real file.
//
// This exists because the guard's scope CHANGED (ADR-0023): it used to reject any
// requirement not on the two-entry allowlist, including `// indirect` ones. That
// was untenable once an allowlisted module grew its own dependency tree — the
// allowlist would have to enumerate the transitive closure, and an allowlist too
// long to read is not a control.
//
// Three properties, all seeded rather than observed on the live file, because the
// live file passing tells you nothing about what the check would REJECT:
//
//   - a DIRECT unreviewed require is still red. This is the mutation control; a
//     guard that passes everything is worse than none;
//   - an INDIRECT unreviewed require is green. This is the narrowing itself;
//   - a direct require on a NON-github host is red. This closes a pre-existing
//     gap: the scan only matched lines beginning `github.com/`, so a
//     `golang.org/x/…` or `go.opentelemetry.io/…` direct require was invisible to
//     it, while the import half (which tests for a dot in the first path segment)
//     would have caught the import.
func TestGoModGuardScope(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mod     string
		wantBad []string
	}{
		{
			name: "the real allowlisted requires are accepted",
			mod: `module github.com/openbox-ai/openbox-shift-left/gateway

go 1.27.0

require (
	github.com/openbox-ai/openbox-shift-left/client v0.0.0
	github.com/openbox-ai/openbox-shift-left/decision v0.0.0
)
`,
		},
		{
			name: "a direct unreviewed require is REJECTED",
			mod: `module github.com/openbox-ai/openbox-shift-left/gateway

require (
	github.com/openbox-ai/openbox-shift-left/client v0.0.0
	github.com/some/unreviewed v1.2.3
)
`,
			wantBad: []string{"github.com/some/unreviewed"},
		},
		{
			name: "an INDIRECT unreviewed require is accepted (the narrowing)",
			mod: `module github.com/openbox-ai/openbox-shift-left/gateway

require github.com/openbox-ai/openbox-shift-left/decision v0.0.0

require (
	github.com/zricethezav/gitleaks/v8 v8.30.1 // indirect
	golang.org/x/text v0.14.0 // indirect
	go.opentelemetry.io/collector/pdata v1.0.0 // indirect
)
`,
		},
		{
			name: "a direct NON-github require is REJECTED (the gap this closes)",
			mod: `module github.com/openbox-ai/openbox-shift-left/gateway

require (
	golang.org/x/text v0.14.0
	go.opentelemetry.io/collector/pdata v1.0.0
)
`,
			wantBad: []string{"golang.org/x/text", "go.opentelemetry.io/collector/pdata"},
		},
		{
			name: "single-line require form is scanned too",
			mod: `module github.com/openbox-ai/openbox-shift-left/gateway

require github.com/some/unreviewed v1.2.3
`,
			wantBad: []string{"github.com/some/unreviewed"},
		},
		{
			name: "the module's own path is not a requirement",
			mod: `module github.com/openbox-ai/openbox-shift-left/gateway

go 1.27.0
`,
		},
		{
			name: "a replace directive is not a requirement",
			mod: `module github.com/openbox-ai/openbox-shift-left/gateway

replace github.com/some/unreviewed => ../unreviewed
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := unallowedDirectRequires(tc.mod, allowedNonStdlibImports)
			if len(got) != len(tc.wantBad) {
				t.Fatalf("got %v, want %v", got, tc.wantBad)
			}
			for _, want := range tc.wantBad {
				found := false
				for _, g := range got {
					if g == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected %q to be rejected; got %v", want, got)
				}
			}
		})
	}
}

// The live go.mod must pass its own guard — the fixtures above prove the check
// discriminates, this proves the module currently complies.
func TestLiveGoModPassesTheGuard(t *testing.T) {
	mod := readGoMod(t)
	if bad := unallowedDirectRequires(mod, allowedNonStdlibImports); len(bad) > 0 {
		t.Errorf("gateway/go.mod has direct requires the allowlist does not name: %v", bad)
	}
}

// An indirect require in the LIVE file is intentionally not an error, and this
// records that it is a real situation rather than a hypothetical one — so the
// narrowing is not mistaken for dead code.
func TestNarrowingAppliesToTheLiveFile(t *testing.T) {
	mod := readGoMod(t)
	if !strings.Contains(mod, "// indirect") {
		t.Skip("gateway/go.mod currently has no indirect requires; the narrowing " +
			"is still correct but nothing here exercises it")
	}
	if bad := unallowedDirectRequires(mod, allowedNonStdlibImports); len(bad) > 0 {
		t.Errorf("indirect requires are leaking into the direct check: %v", bad)
	}
}
