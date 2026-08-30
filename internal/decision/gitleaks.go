package decision

import (
	"sort"
	"strings"
	"sync"

	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/report"
)

// gitleaks supplies the NAMED-FORMAT half of detection (D-OSS-4): 222 maintained
// rules from its embedded default config, replacing nine hand-rolled regexes.
//
// What it does NOT replace, deliberately:
//
// - the generic `secret_assignment` value-group pattern, because gitleaks REPORTS
// findings and does not structurally rewrite a JSON body. Our replacement keeps
// the key and its quoting and trims trailing structural terminators back out of
// the placeholder — logic whose two directions have already shipped broken
// together once, pinned by TestRedact_ValueEndingInBackslash and
// TestRedact_JSONShapedSecrets; - `redactEntropy`, because gitleaks' entropy is a
// per-rule threshold layered on a regex match, not a standalone high-entropy
// scan. Dropping our generic fallback would LOSE coverage on the exact axis
// adopting gitleaks was meant to improve: an unlabelled high-entropy value beside
// an unrecognized key.
//
// Cost, measured before this was written rather than after (phase 06 step 2): the
// CLI binary grows 8,528,818 → 11,258,962 bytes (+32%), and this module's
// reachable package set grows 200 → 379, linking viper, afero, fsnotify,
// mholt/archives, lipgloss, termenv and zerolog. viper is reachable because
// NewDetectorDefaultConfig uses it purely to unmarshal a static //go:embed-ed
// TOML string. That is an accepted cost, ruled on by the owner with the numbers
// in hand; that decision is where the widened transitive surface of THIS module
// is argued, and decision/guard_test.go is what keeps its direct surface
// enumerated.

// gitleaksDetector returns the process-wide detector, built exactly once.
//
// Built once because Redact runs on every gated tool call and on turn text, and
// constructing it compiles 222 rules AND mutates package-global viper state
// (NewDetectorDefaultConfig calls viper.SetConfigType / viper.ReadConfig on the
// global instance). Once-only construction is what keeps that global mutation a
// startup detail rather than a data race on the enforce path.
//
// Concurrency: DetectString builds a local findings slice and returns it — it
// does not accumulate into Detector.findings — and its only mutation of the
// detector is an atomic byte counter. So one shared instance is safe for the
// parallel callers this package promises to support (TestRedact_ConcurrentSafe
// under -race is the check).
var gitleaksDetector = sync.OnceValue(func() *detect.Detector {
	d, err := detect.NewDetectorDefaultConfig()
	if err != nil {
		// Degrade rather than panic: this runs inside a hook binary, and taking
		// the developer's tool down is worse than falling back to the generic
		// assignment pattern plus the entropy pass.
		//
		// This cannot happen in practice — the config is a static embedded string,
		// so failure is deterministic and not input-dependent — and
		// TestGitleaksDetectorBuilds is what makes "cannot happen" checkable
		// instead of asserted. If that test is ever red, detection has silently
		// lost its named-format half.
		return nil
	}
	// Redact is gitleaks' own report-masking flag; it would mask Secret in the
	// findings we need to locate. Leave it off — our replacement is what redacts.
	d.Redact = 0
	d.MaxDecodeDepth = 0
	d.MaxArchiveDepth = 0
	return d
})

// minGitleaksSecretLen is the shortest finding this will act on.
//
// Findings are located by SEARCHING FOR THE SECRET TEXT rather than by arithmetic
// on Finding.StartColumn/EndColumn, which are offsets within Finding.Line and not
// into the whole input — treating them as global offsets silently splices a
// multi-line body at the wrong position, and our inputs are arbitrary multi-line
// bodies (a tool_response is JSON; a file body is a whole file). Searching has no
// such failure mode.
//
// The floor is what keeps searching safe: replacing every occurrence of a short,
// common string would corrupt unrelated text. Below it a finding is ignored, which
// under-redacts — the direction this detector is documented to err in.
const minGitleaksSecretLen = 8

// redactGitleaks replaces every gitleaks finding's secret text with a placeholder,
// recording each rule id as a category.
//
// Categories are gitleaks RULE IDS, content-free by construction — they name the
// rule, never the matched text. That is load-bearing: RedactionCategories reaches
// the durable audit under INV-2, and TestGitleaksCategoriesCarryNoSecret asserts
// it rather than trusting it.
func redactGitleaks(d *detect.Detector, text string, catSet map[string]struct{}) string {
	if d == nil || text == "" {
		return text
	}
	out := text
	// LONGEST FIRST, and the order is a correctness property rather than a
	// preference. Two rules can match overlapping spans of one credential — a
	// generic rule on a substring, a named-format rule on the whole thing — and
	// detector order is not length order. Replacing the SHORT one first destroys
	// the long one's text, so the `!strings.Contains` guard below then skips it as
	// "already covered" and the remaining head and tail of a real secret survive
	// on the wire. Replacing the long one first cannot lose the short one: either
	// it was inside the replaced span, or it is still present and gets its own
	// placeholder.
	all := findings(d, text)
	sort.SliceStable(all, func(i, j int) bool {
		return len(secretText(all[i])) > len(secretText(all[j]))
	})
	for _, f := range all {
		secret := secretText(f)
		if len(secret) < minGitleaksSecretLen {
			continue
		}
		// Never re-redact an already-inserted placeholder, and never act on a
		// secret that IS one — which is how idempotence is preserved across the
		// three passes.
		if strings.Contains(secret, redactedPrefix) {
			continue
		}
		if !strings.Contains(out, secret) {
			// Already replaced by an earlier finding whose span covered this one.
			continue
		}
		catSet[f.RuleID] = struct{}{}
		out = strings.ReplaceAll(out, secret, placeholder(f.RuleID))
	}
	return out
}

// secretText is the text a finding asks us to remove. Secret is what the rule
// captured; Match is the whole matched span, which is what a rule with no capture
// group reports.
func secretText(f report.Finding) string {
	if f.Secret != "" {
		return f.Secret
	}
	return f.Match
}

// findings runs the detector, isolating the call so a panic inside a third-party
// rule engine cannot take down the hook.
//
// The enforce path rewrites developer files and blocks tool calls; a panic here
// would surface as the tool dying, with no indication that a regex engine was the
// cause. Recovering degrades to the generic pattern plus the entropy pass, which
// is the same posture as a detector that failed to build.
func findings(d *detect.Detector, text string) (out []report.Finding) {
	defer func() {
		if r := recover(); r != nil {
			out = nil
		}
	}()
	return d.DetectString(text)
}
