package decision

import (
	"regexp"
	"strings"
	"testing"

	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
)

// The detector must actually construct.
//
// gitleaksDetector degrades to nil on error rather than panicking, because it runs
// inside a hook binary and taking the developer's tool down is worse than falling
// back to the assignment pattern plus the entropy pass. That degradation is
// invisible at runtime — detection would quietly lose its named-format half — so
// this test is what makes "cannot happen" checkable. The config is a static
// embedded string, so if this is green the production path is green too.
func TestGitleaksDetectorConstructs(t *testing.T) {
	d := gitleaksDetector()
	if d == nil {
		t.Fatal("gitleaks detector is nil — named-format detection is silently OFF")
	}
	if n := len(d.Config.Rules); n < 100 {
		t.Errorf("only %d rules loaded; the embedded default config should carry 200+", n)
	}
}

// Categories reaching the durable audit must be rule identifiers, never matched
// text (INV-2). gitleaks rule ids satisfy this by construction — assert it rather
// than trust it, because RedactionCategories is egressed.
func TestGitleaksCategoriesCarryNoSecret(t *testing.T) {
	// A GitLab PAT: a format the nine restored named patterns do NOT cover, so this
	// exercises gitleaks specifically rather than our own floor (which runs first
	// and would otherwise win on, say, an AWS key).
	//
	// Assembled from parts, and the variable is NOT named `secret`/`token`/`key`,
	// deliberately. Writing `secret := "<value>"` on one line got this very file
	// rewritten on disk: that shape matches the generic assignment pattern, so the
	// enforce-path redactor replaced the fixture with a placeholder and the test
	// then asserted nothing. The corruption risk this module documents is real
	// enough to have hit its own test.
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
			t.Errorf("category %q carries secret material — INV-2 violation", c)
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

// The placeholder must stay a usable env-var reference.
//
// The whole point of the env-var shape is that a developer can act on it — export
// the value instead of inlining it. gitleaks rule ids are hyphenated slugs
// ("aws-access-token", "1password-secret-key"), and a hyphen is not legal in a
// shell identifier, so emitting one verbatim would produce a placeholder nobody
// can export. A leading digit needs no handling: the category is appended after
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

// TestOverlappingFindingsCannotLeaveSecretFragments pins the ORDER findings are
// applied in, which is a correctness property and not a preference.
//
// Two rules can match overlapping spans of one credential — a generic rule on a
// substring, a named-format rule on the whole thing — and the detector does not
// return them longest-first. Replacing the SHORT one first destroys the long
// one's text, so the "already covered by an earlier finding" guard skips it, and
// the head of a real secret goes out unredacted: measured as
// `AKIA${OPENBOX_REDACTED_SHORT_TOKEN}` before this was ordered.
//
// Two synthetic rules rather than a pair from the 222: the property is about the
// engine's replacement loop, and pinning it to whichever maintained rules happen
// to overlap today would make this test a hostage to a rule-pack refresh.
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
