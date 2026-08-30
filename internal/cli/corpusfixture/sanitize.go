// Package corpusfixture sanitizes recorded session traffic into committable test
// fixtures, and scans committed fixtures for anything that should not have
// survived.
//
// It exists because the evidence this repository needs most — proof that the
// telemetry and transport lanes map REAL provider traffic correctly — can only
// come from real recorded traffic, and real recorded traffic carries the
// developer's email, account and organization ids, home paths and bearer tokens.
// A fixture is committed, so an unsanitized one is a credential leak into git
// history, which is the hardest place to purge anything from.
//
// Two halves, and they run at different times. Sanitize runs ONCE, by hand,
// against a corpus that is not in this repository (see cmd/corpusfixture). Scan
// runs FOREVER, in CI, against the committed fixtures — so it must be able to
// find a violation on its own, without having been told what Sanitize did.
//
// The failure mode worth naming: a sanitizer that erases too much. The telemetry
// mapper drops any record whose session id or request id fails its charset check
// and any record with no timestamp, so a placeholder like "<redacted>" turns
// every replay fixture into a dropped record — after which the replay suite
// passes, asserts nothing about the mapping, and looks exactly like a suite that
// works. Placeholders here are therefore SHAPE-PRESERVING, and
// TestSanitizePreservesWhatTheMapperParses is what holds that.
package corpusfixture

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// uuidKeys are keys whose corpus values are UUIDs or UUID-like account handles.
// Each distinct real value is replaced by a distinct synthetic UUID, because
// collapsing them would merge two sessions (or two agents) into one and a replay
// fixture would stop exercising the thing that separates them.
//
// Matched case-insensitively: the same identity arrives as an OTLP attribute
// (`session.id`), as an HTTP request header (`x-claude-code-session-id`) and as a
// response header (`anthropic-organization-id`), and a case-sensitive set would
// silently miss whichever spelling the recorder happened to use.
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

// tokenKeys are keys whose values become part of event IDENTITY downstream —
// activity_id is built from the request id — so their placeholders must satisfy
// the mapper's charset rule (letters, digits, '-', '_', '.'; never ':' or '/').
var tokenKeys = map[string]bool{
	"request_id":          true,
	"client_request_id":   true,
	"x-client-request-id": true,
}

// hexShapeKeys are OTLP protocol fields whose values are VALIDATED as hex of an
// exact length by the collector's own JSON unmarshaler. Replacing one with the
// ordinary placeholder makes the whole fixture undecodable — and undecodable is
// the failure mode that matters here, because the entry-point rule for the
// telemetry replay is that a fixture must be read by the PRODUCTION unmarshal.
// A fixture that only our own reader accepts proves nothing about the intake.
//
// The pseudonym is all-DIGIT and the same length, which is simultaneously valid
// hex and invisible to the hex-identifier sentinel below (that rule requires a
// hex letter, precisely so a 19-digit nanosecond timestamp is not mistaken for
// an id). So no allowlist entry is needed to keep Scan quiet about them.
var hexShapeKeys = map[string]bool{
	"spanid":       true,
	"traceid":      true,
	"parentspanid": true,
}

// fixedKeys are keys with no downstream parser, replaced by one constant each.
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

// contentKeys are keys whose values are recorded FREE TEXT — a prompt, a model
// reply, a tool's arguments or its output.
//
// They get filler rather than a pseudonym because there is nothing to preserve.
// A pseudonym exists so two distinct real values stay distinct downstream, and
// nothing downstream reads these: the telemetry lane's body ingestion is
// deferred, so the value is carried and never parsed. What free text does carry
// is whatever the recording machine had in front of the model, which has no
// shape and so cannot be scanned for.
var contentKeys = map[string]bool{
	"prompt":          true,
	"response":        true,
	"tool_input":      true,
	"tool_parameters": true,
}

// systemReminder is the tag the provider wraps injected context in, and it is
// the mechanism by which a developer's global configuration file reached a
// committed fixture: the first prompt of a session carries that file inside one
// of these blocks. Naming the tag names the vector without naming anyone.
const systemReminder = "<system-reminder"

// Value patterns. These run over EVERY string in the document, not only the ones
// under a known key, because the same identity leaks through free text: a home
// path inside a tool_input, an email inside an assistant reply, a token pasted
// into a prompt.
var (
	homePathRe = regexp.MustCompile(`(/Users/|/home/)[A-Za-z0-9._\-]+(?:/[A-Za-z0-9._\-]+)*`)
	emailRe    = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	apiKeyRe   = regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{16,}`)
	bearerRe   = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{12,}`)

	// The two VALUE-SHAPE rules, and they exist because the key-driven rules
	// above provably were not enough. The manual review of the first generated
	// fixtures found a 60-hex-character account identifier sitting inside a
	// response BODY — under the key "body", in free text, where no key name will
	// ever reach it — along with real message and request UUIDs in the same
	// place. A keyword-driven scan misses exactly what it does not name, which is
	// the limit this repository already documents for its own secret detector;
	// these two rules are the shape-driven backstop beneath it.
	//
	// hexIDRe deliberately requires BOTH a hex letter and a digit. A 19-digit
	// nanosecond timestamp is a valid hex charset run, and scrubbing every
	// timeUnixNano would destroy the one field the telemetry mapper needs most.
	// opaqueIDRe covers the provider's prefixed identifiers — tool-use ids,
	// message ids, code-session ids, account ids. Like the two rules below it,
	// this is a VALUE rule rather than a key rule because these ids appear inside
	// request URLs and response bodies as often as they appear under a key of
	// their own.
	//
	// The 12-character floor is what separates an identifier from an enum: the
	// corpus carries `org_level` as a rate-limit reason, and a rule that ate it
	// would silently change what a replay asserts about rate-limit handling.
	opaqueIDRe = regexp.MustCompile(`\b(toolu|msg|cse|acct|user|org|sess|req)_([A-Za-z0-9]{12,})\b`)

	uuidValueRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	hexIDRe     = regexp.MustCompile(`\b(?:[0-9a-f]*[a-f][0-9a-f]*[0-9][0-9a-f]*|[0-9a-f]*[0-9][0-9a-f]*[a-f][0-9a-f]*)\b`)
)

// hexIDMin is the length at or above which an all-hex run reads as an
// identifier rather than as data. 16 is the shortest identifier the corpus
// actually carries (an OTLP spanId, and a plugin_id_hash beside it).
const hexIDMin = 16

// redactedMarkerRe catches this repository's OWN enforce-path redactor having
// rewritten a fixture on the way to disk.
//
// It is not a privacy sentinel; it is a realism one, and it belongs to Scan
// alone — Sanitize must never "repair" it. This repo's redactor has silently
// rewritten four developer files mid-session, including a Go source file whose
// Ed25519 test vector became ${OPENBOX_REDACTED_ENTROPY}. A fixture carrying one
// of these markers where a body used to be still parses, still replays, and
// makes every downstream assertion a statement about the accident rather than
// about the product. Loud is the only useful behaviour.
var redactedMarkerRe = regexp.MustCompile(`\$\{OPENBOX_REDACTED[A-Z_]*\}`)

const (
	homePathPlaceholder = "${1}fixture/project"
	emailPlaceholder    = "fixture@example.invalid"
	apiKeyPlaceholder   = "sk-ant-fixture"
	bearerPlaceholder   = "Bearer fixture-token"
)

// uuidPlaceholderRe and tokenPlaceholderRe describe what a sanitized value looks
// like. Scan asserts against these SHAPES rather than against a list of the real
// values, which is the point: a sentinel list containing the real organization id
// would leak the very thing it exists to keep out.
var (
	uuidPlaceholderRe  = regexp.MustCompile(`^00000000-0000-4000-8000-[0-9a-f]{12}$`)
	tokenPlaceholderRe = regexp.MustCompile(`^req_fixture[0-9]{6}$`)
)

// hexPlaceholder is deliberately NOT all-hex: "hixture" carries three characters
// outside [0-9a-f], so a placeholder can never be mistaken for the thing it
// replaced and Scan needs no allowlist to tell them apart.
func hexPlaceholder(n int) string { return fmt.Sprintf("hixture%09d", n) }

var hexPlaceholderRe = regexp.MustCompile(`^hixture[0-9]{9}$`)

// opaquePlaceholderRe matches the SUFFIX minted above, because the prefix is
// preserved and re-attached by the caller.
var opaquePlaceholderRe = regexp.MustCompile(`^fixture[0-9]{6}$`)

func uuidPlaceholder(n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", n)
}

func tokenPlaceholder(n int) string {
	return fmt.Sprintf("req_fixture%06d", n)
}

// Sanitize rewrites one JSON document so that no real identity survives, while
// every field a consumer parses keeps its shape.
//
// Pseudonyms are assigned per key in first-seen document order, so the same real
// value maps to the same placeholder throughout one document and two distinct
// values stay distinct. A value that is ALREADY a placeholder is left alone and
// consumes no counter, which is what makes a second pass a no-op — fixtures get
// regenerated, and a sanitizer whose output churns turns every regeneration into
// an unreviewable diff.
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
	// assigned maps a placeholder CLASS (not a key) to real-value → placeholder.
	// Class rather than key so that the same session id arriving once as an OTLP
	// attribute and once as an HTTP header gets the SAME pseudonym; keyed by key
	// name they would diverge, and a fixture whose request header disagreed with
	// its telemetry attribute would be quietly unusable for any cross-lane check.
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

// walk rewrites one node. key is the object key this node was reached under, or
// the OTLP attribute name when the node is an attribute's value object.
func (s *sanitizer) walk(node any, key string) any {
	switch v := node.(type) {
	case map[string]any:
		// The OTLP attribute shape: {"key":"session.id","value":{"stringValue":…}}.
		// Without this the walk sees the literal key "value" and every real
		// corpus attribute survives untouched — which passes a test written
		// against plain objects and sanitizes nothing at all.
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
			// stringValue/intValue carry the attribute's value, so they inherit
			// the attribute's name rather than introducing one of their own.
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
	// A recorded request body carries the prompt, so its free text is replaced
	// before the value patterns run: what survives that substitution is
	// structure, and the patterns exist for identifiers hiding in structure.
	if lk == "body" {
		val = SubstitutePromptText(val)
	}
	return s.scrubText(val)
}

// scrubText applies the value patterns.
//
// Order is load-bearing in two places. The api-key pattern runs before the
// bearer pattern, since an Authorization value can carry either and the looser
// bearer rule would otherwise swallow the more specific match. And the UUID rule
// runs before the hex rule, so a UUID becomes a UUID-shaped pseudonym rather than
// four unrelated hex fragments — the mapper and the fixture reader both expect a
// UUID where the corpus had one.
// sameLengthDigits mints an all-digit pseudonym of the input's own length, so a
// length-validated hex field keeps both its length and its charset.
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

// Scan reports every sentinel violation in a document.
//
// It is deliberately independent of Sanitize: it re-derives what a clean fixture
// looks like from the placeholder SHAPES and the value patterns, so a fixture
// hand-edited after sanitization, or produced by an older sanitizer, is still
// caught. A scanner that merely asked "did Sanitize run?" would pass on both.
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
	// The check is the substitution itself rather than a second description of
	// what filler looks like: a rule stated twice is a rule that drifts, and the
	// half that drifts here is the one admitting fixtures.
	if lk == "body" && SubstitutePromptText(val) != val {
		*out = append(*out, Violation{Path: path, Kind: "recorded prompt text in a model-call body"})
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

	// The two shape rules. They run over every value regardless of key, which is
	// the whole point: the residue that motivated them — a 60-hex account
	// identifier and three real UUIDs — sat inside a response BODY, where the key
	// name is "body" and carries no information at all.
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
