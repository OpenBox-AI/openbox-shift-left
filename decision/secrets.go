package decision

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// Tier-1 local secret/entropy detection (STORY-E6-S9, design sidecar-policy-sync.md
// §7 + OD-SYNC-10). Given a content string (a file body carried on the LOCAL
// DecisionRequest.Content), the detector returns a redacted copy in which each
// detected secret is replaced by an env-var-style placeholder, plus the category
// names that fired. It is the redaction SOURCE the E6-S4 `updatedInput` apply path
// was built to consume — the "fourth verdict" (design §7 T1: redact-and-continue).
//
// PROPERTIES (all load-bearing):
//   - DETERMINISTIC + STATELESS + CONCURRENCY-SAFE: the pattern set is compiled once
//     at package init; Redact reads no shared mutable state, so the server can call
//     it from parallel connection handlers with no lock (INV-3b: no I/O either).
//   - LOCAL-ONLY (INV-1/INV-2): it never logs the content or the secret, never
//     performs I/O. Its output rides ONLY the LOCAL Decision.RedactedContent
//     (never client.Evaluation → never egress); category names (never the secret)
//     are the only thing that reaches the durable audit.
//   - PLACEHOLDER = env-var ref (design §7): a secret → `${OPENBOX_REDACTED_<CAT>}`.
//     This deliberately deviates from the guardrails-api PII/ban-list masking styles
//     (`<ENTITY_TYPE>` / `*`×len): for a *secret*, the right nudge is to externalize
//     the value into an env var, not to blank it — the redacted body is what the
//     tool then actually writes (redact-and-continue), so an env-var ref keeps it
//     legible and points the developer at the fix.
//
// The pattern set is intentionally CONSERVATIVE (high-confidence named formats +
// a high-entropy base64-class fallback) to keep false positives low — a false
// positive silently rewrites a developer's file, so the detector errs toward
// missing a secret over corrupting a legitimate write (AC-6).

// Redaction placeholders and thresholds.
const (
	// redactedPrefix is the stable marker every placeholder starts with; the entropy
	// pass and the generic-assignment pass both skip tokens that already contain it
	// so an inserted placeholder is never re-redacted.
	redactedPrefix = "OPENBOX_REDACTED"

	// minAssignmentValueLen is the shortest KEY=VALUE value the generic-assignment
	// pattern will redact — below this it is likely a flag/enum, not a secret.
	minAssignmentValueLen = 8

	// minEntropyTokenLen / entropyThreshold tune the generic high-entropy fallback.
	// 24 chars at ≥4.5 bits/char catches base64-class tokens (charset 64, max 6.0
	// bits/char — real random keys land ~5.5–6.0) while EXCLUDING hex/UUID/git-SHA
	// tokens: hex has a 4.0-bit ceiling (16 symbols) so it can never reach 4.5, and
	// a UUID is hex+dashes — so ordinary hashes/ids are never flagged. This is the
	// low-false-positive posture (AC-6): the fallback only fires on long, genuinely
	// high-diversity tokens.
	minEntropyTokenLen = 24
	entropyThreshold   = 4.5
)

// namedPattern is one high-confidence secret format. When valueGroup == 0 the whole
// match is replaced by the placeholder; when valueGroup > 0 only that submatch group
// (the VALUE of a KEY=VALUE assignment) is replaced, so the key and quoting survive.
type namedPattern struct {
	category   string
	re         *regexp.Regexp
	valueGroup int
}

// secretDetector holds the compiled pattern set. Immutable after construction.
type secretDetector struct {
	patterns []namedPattern
}

// defaultSecretDetector is the process-wide detector (compiled once). Safe for
// concurrent use.
var defaultSecretDetector = newSecretDetector()

func newSecretDetector() *secretDetector {
	return &secretDetector{patterns: []namedPattern{
		// PEM private-key block (multiline). Whole block → placeholder.
		{category: "private_key", re: regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)},
		// AWS access key id (AKIA/ASIA/… + 16 upper-alnum).
		{category: "aws_key", re: regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA|A3T)[0-9A-Z]{16}\b`)},
		// GitHub tokens (ghp_/gho_/ghs_/ghr_/ghu_ + 36+; and fine-grained github_pat_).
		{category: "github_token", re: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`)},
		{category: "github_token", re: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`)},
		// Slack token.
		{category: "slack_token", re: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},
		// Google API key.
		{category: "google_api_key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
		// Stripe secret / restricted key.
		{category: "stripe_key", re: regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[0-9A-Za-z]{16,}\b`)},
		// OpenAI / Anthropic-style secret keys (sk- / sk-ant-).
		{category: "ai_api_key", re: regexp.MustCompile(`\bsk-(?:ant-)?[A-Za-z0-9_\-]{20,}\b`)},
		// JWT (three base64url segments).
		{category: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{5,}\.eyJ[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}\b`)},
		// Generic KEY=VALUE / KEY: VALUE assignment — redact the VALUE only (group 2),
		// keeping the key + surrounding quote. Case-insensitive on the key names.
		{category: "secret_assignment", valueGroup: 2, re: regexp.MustCompile(`(?i)((?:api[_-]?key|secret|token|password|passwd|pwd|access[_-]?key|auth[_-]?token|client[_-]?secret)\s*[:=]\s*["']?)([^\s"',;]{8,})(["']?)`)},
	}}
}

// Redact scans text and returns (redacted, categories, changed). categories is the
// sorted, de-duplicated set of pattern categories that fired (never the secret
// itself — INV-2). changed reports whether any redaction was applied; when false,
// redacted == text and categories is nil (so the caller can cheaply skip a no-op).
func (d *secretDetector) Redact(text string) (redacted string, categories []string, changed bool) {
	if text == "" {
		return text, nil, false
	}
	catSet := map[string]struct{}{}
	out := text
	for i := range d.patterns {
		p := d.patterns[i]
		out = p.re.ReplaceAllStringFunc(out, func(m string) string {
			if p.valueGroup == 0 {
				catSet[p.category] = struct{}{}
				return placeholder(p.category)
			}
			// Value-group redaction: replace only the VALUE, keep key+quotes.
			loc := p.re.FindStringSubmatchIndex(m)
			g := p.valueGroup
			if loc == nil || len(loc) < 2*(g+1) || loc[2*g] < 0 {
				return m // group did not participate → leave untouched (safe)
			}
			val := m[loc[2*g]:loc[2*g+1]]
			// Never re-redact an already-inserted placeholder, and skip too-short values.
			if len(val) < minAssignmentValueLen || strings.Contains(val, redactedPrefix) {
				return m
			}
			catSet[p.category] = struct{}{}
			return m[:loc[2*g]] + placeholder(p.category) + m[loc[2*g+1]:]
		})
	}
	out = d.redactEntropy(out, catSet)
	if out == text {
		return text, nil, false
	}
	return out, sortedCategories(catSet), true
}

// redactEntropy is the generic fallback: it replaces long, high-entropy base64-class
// tokens no named pattern matched, but ONLY when the token sits in a VALUE POSITION
// — immediately after an `=` or `:` (through any quoting) — i.e. it looks assigned
// (`ANYKEY=<random>`, `X-Token: <random>`), not embedded blob data. This assignment
// gate is the "err toward missing over corrupting" guarantee for the entropy pass
// (G3 Finding 2): free-floating base64 — data: URIs, PEM certificate lines, minified
// bundles, test fixtures — is NOT preceded by an assignment delimiter, so it is left
// intact. It walks the string once; placeholders already inserted are skipped.
func (d *secretDetector) redactEntropy(text string, catSet map[string]struct{}) string {
	var b strings.Builder
	b.Grow(len(text))
	n := len(text)
	i := 0
	for i < n {
		if !isSecretChar(text[i]) {
			b.WriteByte(text[i])
			i++
			continue
		}
		j := i
		for j < n && isSecretChar(text[j]) {
			j++
		}
		tok := text[i:j]
		if len(tok) >= minEntropyTokenLen &&
			!strings.Contains(tok, redactedPrefix) &&
			precededByAssignment(text, i) &&
			shannonEntropy(tok) >= entropyThreshold {
			catSet["entropy"] = struct{}{}
			b.WriteString(placeholder("entropy"))
		} else {
			b.WriteString(tok)
		}
		i = j
	}
	return b.String()
}

// precededByAssignment reports whether the byte at start is in a value position:
// the nearest non-space, non-quote byte before it is an assignment delimiter
// (`=` or `:`). It looks back over spaces/tabs and a single layer of quoting so
// `key = "<tok>"` and `key:<tok>` both qualify, while a token at line start or after
// a `,`/`/` (blob data) does not.
func precededByAssignment(text string, start int) bool {
	k := start - 1
	for k >= 0 {
		switch text[k] {
		case ' ', '\t', '"', '\'':
			k--
		case '=', ':':
			return true
		default:
			return false
		}
	}
	return false
}

// isSecretChar reports whether b is part of a base64 token — the charset a
// high-entropy secret is drawn from: [A-Za-z0-9+/]. It EXCLUDES '=' (base64 padding
// only ever trails, so treating it as a break keeps `KEY=<token>` as two tokens so
// the value's assignment context is visible; any trailing `==` is simply left beside
// the placeholder) and '-'/'_' (hyphen/underscore separators are the named patterns'
// job; excluding them avoids swallowing hyphenated prose into one giant token).
func isSecretChar(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z':
		return true
	case b >= 'a' && b <= 'z':
		return true
	case b >= '0' && b <= '9':
		return true
	case b == '+' || b == '/':
		return true
	}
	return false
}

// shannonEntropy returns the per-byte Shannon entropy (bits/char) of s.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	var freq [256]int
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	n := float64(len(s))
	e := 0.0
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		e -= p * math.Log2(p)
	}
	return e
}

// placeholder renders the env-var-ref redaction marker for a category, e.g.
// "aws_key" → "${OPENBOX_REDACTED_AWS_KEY}". Category slugs are already env-var-safe
// ([a-z0-9_]).
func placeholder(category string) string {
	return "${" + redactedPrefix + "_" + strings.ToUpper(category) + "}"
}

// sortedCategories returns the map keys sorted, for a deterministic audit signal.
func sortedCategories(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
