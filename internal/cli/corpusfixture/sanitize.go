// Package corpusfixture sanitizes recorded session traffic into committable
// test fixtures, and scans committed fixtures for anything that should not
// have survived. Scan runs forever, in CI, against the committed fixtures; so
// it must be able to find a violation on its own, without having been told
// what Sanitize did.
package corpusfixture

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

var uuidKeys = map[string]bool{
	"session.id":                    true,
	"organization.id":               true,
	"user.account_uuid":             true,
	"prompt.id":                     true,
	"flow_id":                       true,
	"x-claude-code-session-id":      true,
	"x-claude-code-agent-id":        true,
	"x-claude-code-parent-agent-id": true,
	"x-organization-uuid":           true,
	"anthropic-organization-id":     true,
	"anthropic-workspace-id":        true,
	"x-request-id":                  true,
	"request-id":                    true,
}

var tokenKeys = map[string]bool{
	"request_id":          true,
	"client_request_id":   true,
	"x-client-request-id": true,
}

var hexShapeKeys = map[string]bool{
	"spanid":       true,
	"traceid":      true,
	"parentspanid": true,
}

var fixedKeys = map[string]string{
	"user.email":      "fixture@example.invalid",
	"user.id":         "fixtureuserid",
	"user.account_id": "fixtureaccountid",
	"process.owner":   "fixture",
	"authorization":   "Bearer fixture-token",
	"x-api-key":       "fixture-api-key",
	"cookie":          "fixture=1",
	"set-cookie":      "fixture=1",
	"traceparent":     "00-00000000000000000000000000000001-0000000000000001-01",
	"traceresponse":   "00-00000000000000000000000000000001-0000000000000001-01",
	"if-none-match":   `"fixture"`,
	"cf-ray":          "fixture",
	"server-timing":   "fixture",
}

// contentKeys a pseudonym exists so two distinct real values stay distinct
// downstream, and nothing downstream reads these: the telemetry lane's body
// ingestion is deferred, so the value is carried and never parsed.
var contentKeys = map[string]bool{
	"prompt":          true,
	"response":        true,
	"tool_input":      true,
	"tool_parameters": true,
}

const systemReminder = "<system-reminder"

var (
	homePathRe = regexp.MustCompile(`(/Users/|/home/)[A-Za-z0-9._\-]+(?:/[A-Za-z0-9._\-]+)*`)
	emailRe    = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	apiKeyRe   = regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{16,}`)
	bearerRe   = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{12,}`)

	// opaqueIDRe hexIDRe deliberately requires both a hex letter and a digit.
	opaqueIDRe = regexp.MustCompile(`\b(toolu|msg|cse|acct|user|org|sess|req)_([A-Za-z0-9]{12,})\b`)

	uuidValueRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	hexIDRe     = regexp.MustCompile(`\b(?:[0-9a-f]*[a-f][0-9a-f]*[0-9][0-9a-f]*|[0-9a-f]*[0-9][0-9a-f]*[a-f][0-9a-f]*)\b`)
)

const hexIDMin = 16

// redactedMarkerRe catches this repository's OWN enforce-path redactor having
// rewritten a fixture on the way to disk.
var redactedMarkerRe = regexp.MustCompile(`\$\{OPENBOX_REDACTED[A-Z_]*\}`)

const (
	homePathPlaceholder = "${1}fixture/project"
	emailPlaceholder    = "fixture@example.invalid"
	apiKeyPlaceholder   = "sk-ant-fixture"
	bearerPlaceholder   = "Bearer fixture-token"
)

var (
	uuidPlaceholderRe  = regexp.MustCompile(`^00000000-0000-4000-8000-[0-9a-f]{12}$`)
	tokenPlaceholderRe = regexp.MustCompile(`^req_fixture[0-9]{6}$`)
)

func hexPlaceholder(n int) string { return fmt.Sprintf("hixture%09d", n) }

var hexPlaceholderRe = regexp.MustCompile(`^hixture[0-9]{9}$`)

var opaquePlaceholderRe = regexp.MustCompile(`^fixture[0-9]{6}$`)

func uuidPlaceholder(n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", n)
}

func tokenPlaceholder(n int) string {
	return fmt.Sprintf("req_fixture%06d", n)
}

// Sanitize rewrites one JSON document so that no real identity survives, while
// every field a consumer parses keeps its shape.
func Sanitize(raw []byte) ([]byte, error) {
	var doc any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("corpusfixture: decode: %w", err)
	}

	s := &sanitizer{
		assigned: map[string]map[string]string{},
		next:     map[string]int{},
	}
	out := s.walk(doc, "")

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("corpusfixture: encode: %w", err)
	}
	return append(b, '\n'), nil
}

type sanitizer struct {
	assigned map[string]map[string]string
	next     map[string]int
}

func (s *sanitizer) pseudonym(class, real string, mint func(int) string, shaped *regexp.Regexp) string {
	if shaped.MatchString(real) {
		return real // already sanitized; consume no counter
	}
	if s.assigned[class] == nil {
		s.assigned[class] = map[string]string{}
	}
	if p, ok := s.assigned[class][real]; ok {
		return p
	}
	s.next[class]++
	p := mint(s.next[class])
	s.assigned[class][real] = p
	return p
}

func (s *sanitizer) walk(node any, key string) any {
	switch v := node.(type) {
	case map[string]any:
		if name, ok := v["key"].(string); ok {
			if val, ok := v["value"].(map[string]any); ok {
				out := map[string]any{"key": name}
				out["value"] = s.walk(val, name)
				for k, vv := range v {
					if k != "key" && k != "value" {
						out[k] = s.walk(vv, k)
					}
				}
				return out
			}
		}
		out := make(map[string]any, len(v))
		for k, vv := range v {
			child := k
			if isOTLPValueField(k) && key != "" {
				child = key
			}
			out[k] = s.walk(vv, child)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, vv := range v {
			out[i] = s.walk(vv, key)
		}
		return out
	case string:
		return s.rewrite(key, v)
	default:
		return v
	}
}

func isOTLPValueField(k string) bool {
	switch k {
	case "stringValue", "intValue", "doubleValue", "boolValue":
		return true
	}
	return false
}

func (s *sanitizer) rewrite(key, val string) string {
	lk := strings.ToLower(key)
	switch {
	case uuidKeys[lk]:
		return s.pseudonym("uuid", val, uuidPlaceholder, uuidPlaceholderRe)
	case tokenKeys[lk]:
		return s.pseudonym("token", val, tokenPlaceholder, tokenPlaceholderRe)
	case hexShapeKeys[lk] && val != "":
		return s.sameLengthDigits(val)
	case contentKeys[lk]:
		return SyntheticProse(utf8.RuneCountInString(val))
	}
	if fixed, ok := fixedKeys[lk]; ok {
		return fixed
	}
	if lk == "body" {
		val = SubstituteSSEDeltas(SubstitutePromptText(val))
	}
	return s.scrubText(val)
}

// sameLengthDigits order is load-bearing in two places.
func (s *sanitizer) sameLengthDigits(real string) string {
	if allDigits(real) {
		return real // already a pseudonym; consume no counter
	}
	if s.assigned["hexshape"] == nil {
		s.assigned["hexshape"] = map[string]string{}
	}
	if p, ok := s.assigned["hexshape"][real]; ok {
		return p
	}
	s.next["hexshape"]++
	p := fmt.Sprintf("%0*d", len(real), s.next["hexshape"])
	s.assigned["hexshape"][real] = p
	return p
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func (s *sanitizer) scrubText(v string) string {
	v = apiKeyRe.ReplaceAllString(v, apiKeyPlaceholder)
	v = bearerRe.ReplaceAllString(v, bearerPlaceholder)
	v = emailRe.ReplaceAllString(v, emailPlaceholder)
	v = homePathRe.ReplaceAllString(v, homePathPlaceholder)
	v = opaqueIDRe.ReplaceAllStringFunc(v, func(m string) string {
		prefix := m[:strings.Index(m, "_")]
		return prefix + "_" + s.pseudonym("opaque:"+prefix, m,
			func(n int) string { return fmt.Sprintf("fixture%06d", n) },
			opaquePlaceholderRe)
	})
	v = uuidValueRe.ReplaceAllStringFunc(v, func(m string) string {
		return s.pseudonym("uuid", m, uuidPlaceholder, uuidPlaceholderRe)
	})
	v = hexIDRe.ReplaceAllStringFunc(v, func(m string) string {
		if len(m) < hexIDMin {
			return m
		}
		return s.pseudonym("hex", m, hexPlaceholder, hexPlaceholderRe)
	})
	return v
}

// Violation is one thing a committed fixture must not contain.
type Violation struct {
	// Path is a dotted JSON path to the offending node, best effort.
	Path string
	// Kind names the sentinel class that fired.
	Kind string
}

func (v Violation) String() string { return v.Kind + " at " + v.Path }

// Scan reports every sentinel violation in a document. It is deliberately
// independent of Sanitize: it re-derives what a clean fixture looks like from
// the placeholder shapes and the value patterns, so a fixture hand-edited
// after sanitization, or produced by an older sanitizer, is still caught.
func Scan(raw []byte) []Violation {
	var doc any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return []Violation{{Path: "$", Kind: "unparseable JSON"}}
	}
	var out []Violation
	scanNode(doc, "$", "", &out)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func scanNode(node any, path, key string, out *[]Violation) {
	switch v := node.(type) {
	case map[string]any:
		if name, ok := v["key"].(string); ok {
			if val, ok := v["value"].(map[string]any); ok {
				scanNode(val, path+"."+name, name, out)
				return
			}
		}
		names := make([]string, 0, len(v))
		for k := range v {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			child := k
			if isOTLPValueField(k) && key != "" {
				child = key
			}
			scanNode(v[k], path+"."+k, child, out)
		}
	case []any:
		for i, vv := range v {
			scanNode(vv, fmt.Sprintf("%s[%d]", path, i), key, out)
		}
	case string:
		scanString(v, path, key, out)
	}
}

func scanString(val, path, key string, out *[]Violation) {
	lk := strings.ToLower(key)
	switch {
	case uuidKeys[lk]:
		if !uuidPlaceholderRe.MatchString(val) {
			*out = append(*out, Violation{Path: path, Kind: "unsanitized identity key " + lk})
		}
		return
	case tokenKeys[lk]:
		if !tokenPlaceholderRe.MatchString(val) {
			*out = append(*out, Violation{Path: path, Kind: "unsanitized identity key " + lk})
		}
		return
	}
	if fixed, ok := fixedKeys[lk]; ok {
		if val != fixed {
			*out = append(*out, Violation{Path: path, Kind: "unsanitized identity key " + lk})
		}
		return
	}
	if contentKeys[lk] {
		if val != SyntheticProse(utf8.RuneCountInString(val)) {
			*out = append(*out, Violation{Path: path, Kind: "recorded free text under " + lk})
		}
		return
	}
	if lk == "body" {
		if SubstitutePromptText(val) != val {
			*out = append(*out, Violation{Path: path, Kind: "recorded prompt text in a model-call body"})
		}
		if SubstituteSSEDeltas(val) != val {
			*out = append(*out, Violation{Path: path, Kind: "recorded model reply in an event stream"})
		}
		for _, k := range malformedBlocks(val) {
			*out = append(*out, Violation{Path: path, Kind: k})
		}
	}
	if strings.Contains(val, systemReminder) {
		*out = append(*out, Violation{Path: path, Kind: "injected-context block (carries whatever the recording machine had open)"})
	}
	for _, m := range redactedMarkerRe.FindAllString(val, -1) {
		*out = append(*out, Violation{Path: path, Kind: "redaction marker " + m + " (fixture was rewritten on write)"})
	}
	for _, c := range []struct {
		kind string
		re   *regexp.Regexp
		ok   string
	}{
		{"api key", apiKeyRe, apiKeyPlaceholder},
		{"bearer token", bearerRe, bearerPlaceholder},
		{"email address", emailRe, emailPlaceholder},
		{"home path", homePathRe, ""},
	} {
		for _, m := range c.re.FindAllString(val, -1) {
			if c.kind == "home path" {
				if m == "/Users/fixture/project" || m == "/home/fixture/project" {
					continue
				}
			} else if m == c.ok {
				continue
			}
			*out = append(*out, Violation{Path: path, Kind: c.kind})
		}
	}

	for _, m := range opaqueIDRe.FindAllStringSubmatch(val, -1) {
		if !opaquePlaceholderRe.MatchString(m[2]) {
			*out = append(*out, Violation{Path: path, Kind: "unsanitized " + m[1] + "_ identifier"})
		}
	}
	for _, m := range uuidValueRe.FindAllString(val, -1) {
		if !uuidPlaceholderRe.MatchString(m) {
			*out = append(*out, Violation{Path: path, Kind: "unsanitized UUID"})
		}
	}
	for _, m := range hexIDRe.FindAllString(val, -1) {
		if len(m) >= hexIDMin {
			*out = append(*out, Violation{Path: path, Kind: "unsanitized hex identifier"})
		}
	}
}
