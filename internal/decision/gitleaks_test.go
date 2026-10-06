package decision

import (
	"regexp"
	"strings"
	"testing"

	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
)

// TestGitleaksDetectorConstructs the detector must actually construct. That
// degradation is invisible at runtime; detection would quietly lose its named-
// format half; so this test is what makes "cannot happen" checkable.
func TestGitleaksDetectorConstructs(t *testing.T) {
	d := gitleaksDetector()
	if d == nil {
		t.Fatal("gitleaks detector is nil; named-format detection is silently OFF")
	}
	if n := len(d.Config.Rules); n < 100 {
		t.Errorf("only %d rules loaded; the embedded default config should carry 200+", n)
	}
}

// TestGitleaksCategoriesCarryNoSecret categories reaching the durable audit
// must be rule identifiers, never matched text (INV-2). Gitleaks rule ids
// satisfy this by construction; assert it rather than trust it, because
// RedactionCategories is egressed.
func TestGitleaksCategoriesCarryNoSecret(t *testing.T) {
	// Assembled from parts, and the variable is NOT named `secret`/`token`/`key`,
	// deliberately.
	fixture := "glpat-" + "ABCdef1234567890abcd"
	_, cats, changed := newSecretDetector().Redact("gitlab = " + fixture)
	if !changed {
		t.Fatal("fixture was not redacted, so this asserts nothing")
	}
	if len(cats) == 0 {
		t.Fatal("no categories reported")
	}
	for _, c := range cats {
		if strings.Contains(c, fixture) || strings.Contains(fixture, c) {
			t.Errorf("category %q carries secret material; INV-2 violation", c)
		}
		for i := 0; i < len(c); i++ {
			b := c[i]
			ok := (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
				(b >= '0' && b <= '9') || b == '-' || b == '_' || b == '.'
			if !ok {
				t.Errorf("category %q contains %q, which is not identifier-shaped; a category "+
					"names a rule and must never be free text", c, string(b))
			}
		}
	}
}

// TestPlaceholderIsEnvVarSafe the placeholder must stay a usable env-var
// reference. A leading digit needs no handling: the category is appended after
// "OPENBOX_REDACTED_", so the name never starts with one.
func TestPlaceholderIsEnvVarSafe(t *testing.T) {
	for _, cat := range []string{
		"aws-access-token", "1password-secret-key", "gcp-api-key",
		"secret_assignment", "entropy", "private-key", "a.b-c",
	} {
		got := placeholder(cat)
		if !strings.HasPrefix(got, "${OPENBOX_REDACTED_") || !strings.HasSuffix(got, "}") {
			t.Errorf("placeholder(%q) = %q, wrong shape", cat, got)
			continue
		}
		name := got[2 : len(got)-1]
		for i := 0; i < len(name); i++ {
			b := name[i]
			ok := (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
			if !ok {
				t.Errorf("placeholder(%q) = %q: %q is not valid in an env var name", cat, got, string(b))
			}
		}
		if name[0] >= '0' && name[0] <= '9' {
			t.Errorf("placeholder(%q) = %q starts with a digit", cat, got)
		}
	}
}

// TestOverlappingFindingsCannotLeaveSecretFragments pins the order findings
// are applied in, which is a correctness property and not a preference.
func TestOverlappingFindingsCannotLeaveSecretFragments(t *testing.T) {
	const secret = "AKIAABCDEFGHIJKLMNOPTAIL"
	d := detect.NewDetector(config.Config{Rules: map[string]config.Rule{
		"long-token":  {RuleID: "long-token", Regex: regexp.MustCompile(`AKIA[0-9A-Z]{16}TAIL`)},
		"short-token": {RuleID: "short-token", Regex: regexp.MustCompile(`[0-9A-Z]{16}TAIL`)},
	}})

	got := redactGitleaks(d, "value="+secret+" end", map[string]struct{}{})

	if strings.Contains(got, "AKIA") {
		t.Errorf("a fragment of the credential survived redaction: %q", got)
	}
	if strings.Contains(got, secret) {
		t.Errorf("the credential survived redaction entirely: %q", got)
	}
	if !strings.Contains(got, "OPENBOX_REDACTED") {
		t.Errorf("nothing was redacted: %q", got)
	}
}
