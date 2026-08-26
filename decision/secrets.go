package decision

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// Tier-1 local secret/entropy detection. Given a content string (a file
// body carried on the local DecisionRequest.Content), the detector returns
// a redacted copy in which each detected secret is replaced by an
// env-var-style placeholder, plus the category names that fired. It's the
// redaction source the apply path's `updatedInput` was built to consume —
// the "fourth verdict": redact-and-continue.
//
// Properties (all load-bearing):
//   - Deterministic + stateless + concurrency-safe: the pattern set is
//     compiled once at package init; Redact reads no shared mutable state,
//     so the server can call it from parallel connection handlers with no
//     lock (INV-3b: no I/O either).
//   - Local-only (INV-1/INV-2): it never logs the content or the secret,
//     never performs I/O. Its output rides only the local
//     Decision.RedactedContent (never client.Evaluation → never egress);
//     category names (never the secret) are the only thing that reaches
//     the durable audit.
//   - Placeholder = env-var ref: a secret → `${OPENBOX_REDACTED_<CAT>}`.
//     This deliberately deviates from PII/ban-list masking styles
//     (`<ENTITY_TYPE>` / `*`×len): for a secret, the right nudge is to
//     externalize the value into an env var, not to blank it — the
//     redacted body is what the tool then actually writes, so an env-var
//     ref keeps it legible and points the developer at the fix.
//
// The pattern set is intentionally conservative (high-confidence named
// formats + a high-entropy base64-class fallback) to keep false positives
// low — a false positive silently rewrites a developer's file, so the
// detector errs toward missing a secret over corrupting a legitimate write.

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
		//
		// The `[\\"']*` either side of the delimiter is what makes JSON work. The
		// keyword used to have to sit ADJACENT to the `:`/`=`, so `password=x`
		// matched but `{"password":"x"}` did not — the key's closing quote is in
		// between — and once a JSON value is nested inside another JSON string
		// (which is what a tool_response is) there is an escaping backslash there
		// too. Tool output made that the common shape, not an edge case.
		//
		// The value group is `[^\s"',;]{8,}` — backslashes INCLUDED, because a
		// secret can legitimately contain and end with one (a Windows directory
		// used as a credential value is the everyday case).
		//
		// Two properties have to hold at once here, and the regex can only express
		// one of them:
		//
		//	the secret is redacted whole
		//	the `\` that terminates an escaped JSON string is NOT swallowed —
		//	this text rides inside a JSON body, and eating that backslash leaves
		//	unparseable JSON on the wire
		//
		// Expressing the second in the pattern — requiring the last character not
		// to be a backslash — cost the first, silently: a value of exactly 8
		// characters ending in a backslash then matched NOTHING, because no split
		// satisfies both the 8-char floor and a non-backslash tail. So the boundary
		// lives in the REPLACEMENT step instead (Redact below), which trims
		// trailing backslashes out of the placeholder while still measuring the
		// whole value against the floor. Both directions are pinned:
		// TestRedact_ValueEndingInBackslash and
		// TestRedact_JSONShapedSecrets/escaping_survives.
		//
		// (This comment previously carried a `keyword=value` example of the Windows
		// case and the detector redacted its own documentation, leaving the
		// sentence unreadable. Examples here stay unmatchable — a bare path, never
		// beside a keyword and a delimiter.)
		{category: "secret_assignment", valueGroup: 2, re: regexp.MustCompile(`(?i)((?:api[_-]?key|secret|token|password|passwd|pwd|access[_-]?key|auth[_-]?token|client[_-]?secret)[\\"']*\s*[:=]\s*[\\"']*)([^\s"',;]{8,})(["']?)`)},
	}}
}

// isValueTerminator reports whether b is a byte that ends a value's CONTAINER
// rather than being part of the value.
//
// The generic assignment pattern's value group excludes whitespace, quotes,
// commas and semicolons, so those can never be swallowed. These four can, and
// each one matters somewhere the redacted text has to stay parseable: `\` closes
// an escaped JSON string, `}` and `]` close the object or array an unquoted value
// sits at the end of, and `)` closes a shell or config substitution. Trimming
// them out of the placeholder is what keeps the redactor from turning valid JSON
// into a parse error — which on the enforce path is written to the developer's
// file, not merely logged.
func isValueTerminator(b byte) bool {
	switch b {
	case '\\', '}', ']', ')':
		return true
	}
	return false
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
			// The floor is measured against the WHOLE value, before the backslash trim
			// below — trimming first would push a value that legitimately ends in a
			// separator under the floor and stop redacting it.
			if len(val) < minAssignmentValueLen || strings.Contains(val, redactedPrefix) {
				return m
			}
			// Trailing STRUCTURAL bytes stay OUTSIDE the placeholder. In a JSON body —
			// which is what a tool_response is, and what an unquoted value in a config
			// file looks like — these are the bytes that terminate the value's
			// container, so swallowing one leaves output the consumer cannot parse.
			// None of them carries secret material, so keeping them costs nothing:
			// what is redacted is still the whole value minus its separators.
			//
			// This is the boundary the pattern used to express and could not, because
			// requiring a non-separator tail there made an at-the-floor value
			// invisible: no split satisfies both the 8-character floor and a
			// constrained final byte, so a value of exactly 8 characters ending in one
			// matched NOTHING and a real secret shipped.
			//
			// The set is `\` plus the closers, and the closers are why: the value
			// group excludes `"`, `'`, `,` and `;` but NOT `}` or `]`, so an unquoted
			// value at the end of an object or array — `{"auth_token": ${OPENBOX_REDACTED_SECRET_ASSIGNMENT}
			// — ran its terminator into the match. That became reachable only when the
			// pattern learned to skip the key's quoting, i.e. exactly when JSON started
			// matching at all, so the two changes belong together.
			// TestRedact_JSONTerminatorsSurvive and
			// TestRedact_ValueEndingInBackslash pin both directions.
			end := loc[2*g+1]
			for end > loc[2*g] && isValueTerminator(m[end-1]) {
				end--
			}
			// A value of only separators has nothing to redact. Emitting a placeholder
			// over an empty span would report a redaction that did not happen.
			if end == loc[2*g] {
				return m
			}
			catSet[p.category] = struct{}{}
			return m[:loc[2*g]] + placeholder(p.category) + m[end:]
		})
	}
	out = d.redactEntropy(out, catSet)
	if out == text {
		return text, nil, false
	}
	return out, sortedCategories(catSet), true
}

// redactEntropy is the generic fallback: it replaces long, high-entropy
// base64-class tokens no named pattern matched, but only when the token
// sits in a value position — immediately after an `=` or `:` (through any
// quoting) — i.e. it looks assigned (`ANYKEY=<random>`, `X-Token: <random>`),
// not embedded blob data. This assignment gate is the "err toward missing
// over corrupting" guarantee for the entropy pass: free-floating base64 —
// data: URIs, PEM certificate lines, minified bundles, test fixtures — is
// not preceded by an assignment delimiter, so it is left intact. It walks
// the string once; placeholders already inserted are skipped.
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
// the nearest non-space, non-quote, non-escape byte before it is an assignment
// delimiter (`=` or `:`). It looks back over spaces/tabs, quoting and JSON escapes
// so `key = "<tok>"`, `key:<tok>` and `{\"key\":\"<tok>\"}` all qualify, while a
// token at line start or after a `,`/`/` (blob data) does not.
//
// The backslash is in that skip set because tool output is carried as JSON: a
// nested value arrives escaped, and stopping at the `\` meant the entropy pass
// silently declined to look at any secret inside an MCP result or a
// `cat config.json`. It skipped quotes already for the same reason, one layer
// short.
func precededByAssignment(text string, start int) bool {
	k := start - 1
	for k >= 0 {
		switch text[k] {
		case ' ', '\t', '"', '\'', '\\':
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
