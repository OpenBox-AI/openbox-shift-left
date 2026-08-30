package decision

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/zricethezav/gitleaks/v8/detect"
)

// Properties (all load-bearing): - Deterministic + stateless + concurrency-
// safe: the pattern set is

const (
	redactedPrefix = "OPENBOX_REDACTED"

	minAssignmentValueLen = 8

	minEntropyTokenLen = 24
	entropyThreshold   = 4.5
)

type namedPattern struct {
	category   string
	re         *regexp.Regexp
	valueGroup int
}

type secretDetector struct {
	patterns []namedPattern
	gitleaks *detect.Detector
}

// defaultSecretDetector is the process-wide detector (compiled once). Lazy,
// deliberately.
var defaultSecretDetector = sync.OnceValue(newSecretDetector)

func newSecretDetector() *secretDetector {
	// Tests call newSecretDetector freely, so this must stay cheap.
	return &secretDetector{gitleaks: gitleaksDetector(), patterns: []namedPattern{
		{category: "private_key", re: regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)},
		{category: "aws_key", re: regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA|A3T)[0-9A-Z]{16}\b`)},
		{category: "github_token", re: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`)},
		{category: "github_token", re: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`)},
		{category: "slack_token", re: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},
		{category: "google_api_key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
		{category: "stripe_key", re: regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[0-9A-Za-z]{16,}\b`)},
		{category: "ai_api_key", re: regexp.MustCompile(`\bsk-(?:ant-)?[A-Za-z0-9_\-]{20,}\b`)},
		{category: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{5,}\.eyJ[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}\b`)},
		{category: "secret_assignment", valueGroup: 2, re: regexp.MustCompile(`(?i)((?:api[_-]?key|secret|token|password|passwd|pwd|access[_-]?key|auth[_-]?token|client[_-]?secret)[\\"']*\s*[:=]\s*[\\"']*)([^\s"',;]{8,})(["']?)`)},
	}}
}

// isValueTerminator reports whether b is a byte that ends a value's container
// rather than being part of the value.
func isValueTerminator(b byte) bool {
	switch b {
	case '\\', '}', ']', ')':
		return true
	}
	return false
}

// Redact scans text and returns (redacted, categories, changed). Categories is
// the sorted, de-duplicated set of pattern categories that fired (never the
// secret itself; INV-2). Changed reports whether any redaction was applied;
// when false, redacted == text and categories is nil (so the caller can
// cheaply skip a no-op).
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
			loc := p.re.FindStringSubmatchIndex(m)
			g := p.valueGroup
			if loc == nil || len(loc) < 2*(g+1) || loc[2*g] < 0 {
				return m // group did not participate → leave untouched (safe)
			}
			val := m[loc[2*g]:loc[2*g+1]]
			if len(val) < minAssignmentValueLen || strings.Contains(val, redactedPrefix) {
				return m
			}
			end := loc[2*g+1]
			for end > loc[2*g] && isValueTerminator(m[end-1]) {
				end--
			}
			if end == loc[2*g] {
				return m
			}
			catSet[p.category] = struct{}{}
			return m[:loc[2*g]] + placeholder(p.category) + m[end:]
		})
	}
	// Our own patterns, then gitleaks, then entropy, and the order is
	// load-bearing. gitleaks replaces a finding's text wholesale and does not go
	// through the value-group and terminator-trim path above, so running it
	// first made JSON parseability depend on how it drew its capture group.
	out = redactGitleaks(d.gitleaks, out, catSet)

	out = d.redactEntropy(out, catSet)
	if out == text {
		return text, nil, false
	}
	return out, sortedCategories(catSet), true
}

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

// precededByAssignment reports whether the byte at start is in a value
// position: the nearest non-space, non-quote, non-escape byte before it is an
// assignment delimiter (`=` or `:`).
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
func placeholder(category string) string {
	var b strings.Builder
	b.Grow(len(redactedPrefix) + len(category) + 4)
	b.WriteString("${")
	b.WriteString(redactedPrefix)
	b.WriteByte('_')
	for i := 0; i < len(category); i++ {
		c := category[i]
		switch {
		case c >= 'a' && c <= 'z':
			b.WriteByte(c - 32)
		case (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	b.WriteByte('}')
	return b.String()
}

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
