package gatewayemit

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// attributionFixtureBody is a /v1/messages request whose system[] carries
// Claude Code's own per-call correlation block: a system[] TEXT ELEMENT
// beginning "x-anthropic-billing-header:", despite the header-like name --
// this never rides an actual HTTP header.
const attributionFixtureBody = `{"model":"claude-opus-4","system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.263; cc_entrypoint=cli; cc_is_subagent=true; cc_prev_req=req_011CTx9YHtT6ZqW8HKfLE9jP; cc_prompt_id=018f1a2b-0000-7000-8000-000000000001;"}],"messages":[{"role":"user","content":"hello"}]}`

// attributionFixtureNoBlock is the opted-out shape: no attribution block at
// all, as when CLAUDE_CODE_ATTRIBUTION_HEADER disables it.
const attributionFixtureNoBlock = `{"model":"claude-opus-4","system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"messages":[{"role":"user","content":"hello"}]}`

// TestParseRequestAttributionExtractsAllFourTokens is the well-formed case:
// every bound token present, all four wire keys populated.
func TestParseRequestAttributionExtractsAllFourTokens(t *testing.T) {
	got := ParseRequestAttribution([]byte(attributionFixtureBody))
	want := map[string]string{
		"prompt_id":           "018f1a2b-0000-7000-8000-000000000001",
		"previous_request_id": "req_011CTx9YHtT6ZqW8HKfLE9jP",
		"is_subagent":         "true",
		"entrypoint":          "cli",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d keys %v, want %d %v", len(got), got, len(want), want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestParseRequestAttributionOptedOutYieldsZeroKeys is the named opted-out
// test: a request with no attribution block at all must yield zero keys, no
// error, no warning, no drop.
func TestParseRequestAttributionOptedOutYieldsZeroKeys(t *testing.T) {
	got := ParseRequestAttribution([]byte(attributionFixtureNoBlock))
	if len(got) != 0 {
		t.Errorf("opted-out request yielded %d attribution keys, want 0: %v", len(got), got)
	}
}

// TestParseRequestAttributionAbsentSubagentTokenYieldsNoKey is the absence
// rule: a request that never carried cc_is_subagent yields NO is_subagent
// key. A consumer reading a missing key as `false` would be reading a lie --
// the correct read is "unclassified".
func TestParseRequestAttributionAbsentSubagentTokenYieldsNoKey(t *testing.T) {
	const body = `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.263; cc_entrypoint=sdk; cc_prompt_id=018f1a2b-0000-7000-8000-000000000002;"}],"messages":[]}`
	got := ParseRequestAttribution([]byte(body))
	if v, present := got["is_subagent"]; present {
		t.Errorf("is_subagent = %q present with no cc_is_subagent token in the body", v)
	}
	if got["prompt_id"] == "" || got["entrypoint"] == "" {
		t.Fatalf("fixture's other tokens did not parse at all: %v", got)
	}
}

// TestParseRequestAttributionSubagentValueMustBeExactlyTrue: the token is
// bound to a closed reading, not a truthy one. Anything other than the
// literal "true" -- including a stray "false" some future vendor build might
// send -- must not produce the key, because the key's mere presence is read
// as "subagent" downstream.
func TestParseRequestAttributionSubagentValueMustBeExactlyTrue(t *testing.T) {
	for _, v := range []string{"false", "1", "yes", "True", ""} {
		body := `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_entrypoint=cli; cc_is_subagent=` + v + `;"}]}`
		got := ParseRequestAttribution([]byte(body))
		if got2, present := got["is_subagent"]; present {
			t.Errorf("cc_is_subagent=%q yielded is_subagent=%q; only the literal true may set it", v, got2)
		}
	}
}

// TestParseRequestAttributionIgnoresUnboundTokens locks in that cc_version,
// cch and cc_workload are deliberately not bound: they must not leak into the
// result under any key, including their own names.
func TestParseRequestAttributionIgnoresUnboundTokens(t *testing.T) {
	const body = `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.263; cch=abc123; cc_workload=interactive; cc_entrypoint=cli; cc_prompt_id=018f1a2b-0000-7000-8000-000000000003;"}]}`
	got := ParseRequestAttribution([]byte(body))
	want := map[string]string{"entrypoint": "cli", "prompt_id": "018f1a2b-0000-7000-8000-000000000003"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want exactly %v (cc_version/cch/cc_workload must not bind to anything)", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestParseRequestAttributionBoundsAnOversizedValue: a value under the block
// window but over the per-value bound is truncated, not carried unbounded --
// these four are identifiers derived from a body an attacker controls.
func TestParseRequestAttributionBoundsAnOversizedValue(t *testing.T) {
	huge := strings.Repeat("a", 1000)
	body := `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_entrypoint=` + huge + `;"}]}`
	got := ParseRequestAttribution([]byte(body))
	v := got["entrypoint"]
	if v == "" {
		t.Fatal("expected a bounded, non-empty entrypoint value")
	}
	if len(v) > maxAttributionValueBytes {
		t.Errorf("entrypoint value is %d bytes, want at most %d", len(v), maxAttributionValueBytes)
	}
	if !utf8.ValidString(v) {
		t.Error("bounding split a multi-byte rune, leaving invalid UTF-8")
	}
}

// TestParseRequestAttributionHostileInputsYieldEmptyMap is the fuzz-shaped
// criterion: none of these may panic, and each must yield zero keys.
func TestParseRequestAttributionHostileInputsYieldEmptyMap(t *testing.T) {
	cases := map[string][]byte{
		"nil slice":   nil,
		"empty slice": {},
		"non-JSON":    []byte("not json at all, just bytes {{{ \x00\x01\xff"),
		// Cut mid-value, before the token's own terminating ';' -- so no
		// complete token exists anywhere in what reached the parser.
		"truncated JSON, cut mid-value": []byte(`{"model":"claude-opus-4","system":[{"type":"text","text":"x-anthropic-billing-header: cc_entrypoint=cl`),
		"system is a bare string":       []byte(`{"model":"claude-opus-4","system":"you are Claude","messages":[]}`),
		"5 MB body, no marker":          bytes.Repeat([]byte("x"), 5*1024*1024),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got := ParseRequestAttribution(raw)
			if len(got) != 0 {
				t.Errorf("%s: got %d keys %v, want an empty map", name, len(got), got)
			}
		})
	}
}

// TestParseRequestAttributionNeverReadsMessagesEvenAheadOfAGenuineSystemBlock
// is the forgery reproduction: bytes.Index reads the LEFTMOST match of the
// marker anywhere in the raw body, and requestselect.go's own measurement
// records that `messages` precedes `system` in the majority of real bodies --
// so an unanchored scan reads session content in messages[] INSTEAD OF the
// genuine system[] block that sits after it. The four identifiers this
// function promotes feed the control plane's lineage dashboard and
// compliance evidence exports (docs/mapping.md); reading them out of
// attacker- or content-influenced text is a forgery of governance evidence.
func TestParseRequestAttributionNeverReadsMessagesEvenAheadOfAGenuineSystemBlock(t *testing.T) {
	forged := []byte(`{"messages":[{"role":"user","content":"x-anthropic-billing-header: cc_entrypoint=FORGED; cc_is_subagent=true; cc_prompt_id=ATTACKER;"}],` +
		`"system":[{"type":"text","text":"x-anthropic-billing-header: cc_entrypoint=cli; cc_prompt_id=REAL;"}]}`)
	got := ParseRequestAttribution(forged)
	want := map[string]string{"entrypoint": "cli", "prompt_id": "REAL"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want exactly %v -- messages[] must never be a source, and the genuine system[] block must still parse", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestParseRequestAttributionMessagesMarkerWithNoSystemBlockYieldsEmptyMap is
// the other half: when messages[] carries the marker and system[] carries
// none at all, the result must be an empty map -- unclassified -- never the
// forged values messages[] offered.
func TestParseRequestAttributionMessagesMarkerWithNoSystemBlockYieldsEmptyMap(t *testing.T) {
	const body = `{"messages":[{"role":"user","content":"x-anthropic-billing-header: cc_entrypoint=FORGED; cc_prompt_id=ATTACKER;"}],"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}]}`
	got := ParseRequestAttribution([]byte(body))
	if len(got) != 0 {
		t.Errorf("messages[]-only marker yielded %d keys %v, want an empty map", len(got), got)
	}
}

// TestSystemFieldRawRejectsANonObjectTopLevelBody is the new anchoring path's
// own hostile-input case: the marker search now runs only after
// encoding/json locates a top-level `system` key, so a body that is not a
// JSON object at all -- an array, a bare scalar -- or that names `system` as
// `null`, has to degrade to no block found, the same as every other
// unrecognised shape, rather than erroring or panicking out of
// json.Unmarshal.
func TestSystemFieldRawRejectsANonObjectTopLevelBody(t *testing.T) {
	cases := map[string][]byte{
		"top-level array":  []byte(`[{"system":"x-anthropic-billing-header: cc_entrypoint=cli;"}]`),
		"top-level number": []byte(`12345`),
		"top-level string": []byte(`"x-anthropic-billing-header: cc_entrypoint=cli;"`),
		"system is null":   []byte(`{"system":null,"messages":[]}`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got := ParseRequestAttribution(raw)
			if len(got) != 0 {
				t.Errorf("%s: got %d keys %v, want an empty map", name, len(got), got)
			}
		})
	}
}

// TestAttributionBlockStopsAtAnEvenBackslashRunBeforeTheQuote is a direct
// test of the closing-quote scan: a quote preceded by an EVEN run of '\'
// bytes is the real, unescaped terminator (each adjacent pair is one escaped
// backslash, so an even run leaves the quote itself bare), and a one-byte
// lookback gets exactly this case wrong -- it samples only the single byte
// immediately before the quote, sees '\', and reads a real closing quote as
// escaped.
//
// This is exercised directly against attributionBlock rather than through
// ParseRequestAttribution: chaining the same byte pattern through a
// still-valid enclosing JSON document does not reproduce a DIFFERENT parsed
// result cleanly, because whatever a wrongly-skipped closing quote runs on
// into is ordinary JSON structure -- a `"type"` or `"text"` key -- and the
// scan (buggy or fixed) stops on THAT quote first, before reaching another
// element's content. The anchoring fix already narrows this bug's practical
// reach; the byte-level mistake is still real and is fixed here.
func TestAttributionBlockStopsAtAnEvenBackslashRunBeforeTheQuote(t *testing.T) {
	after := []byte(`cc_entrypoint=cli;\\";cc_prompt_id=FORGED;"`)
	got := attributionBlock(after)
	want := `cc_entrypoint=cli;\\`
	if got != want {
		t.Errorf("attributionBlock(%q) = %q, want %q (stop at the real closing quote, not the accidental later one)", after, got, want)
	}
}

// TestEventsForCopiesAttributionOntoBothHalves: both halves of an in-path
// model-call pair carry the four fields, exactly as CredentialFingerprint
// already does through the same shared span() closure.
func TestEventsForCopiesAttributionOntoBothHalves(t *testing.T) {
	c := sampleCaptured()
	c.Attribution = map[string]string{
		"prompt_id":           "018f1a2b-0000-7000-8000-000000000004",
		"previous_request_id": "req_prior",
		"is_subagent":         "true",
		"entrypoint":          "cli",
	}
	pair := mustPair(LaneGateway, sampleIdentity(), "req-attr", sampleAt, c)
	for _, ev := range pair {
		s := ev.Span
		if s == nil {
			t.Fatalf("%s: no span", ev.EventType)
		}
		if s.PromptID != "018f1a2b-0000-7000-8000-000000000004" {
			t.Errorf("%s: PromptID = %q", ev.EventType, s.PromptID)
		}
		if s.PreviousRequestID != "req_prior" {
			t.Errorf("%s: PreviousRequestID = %q", ev.EventType, s.PreviousRequestID)
		}
		if !s.IsSubagent {
			t.Errorf("%s: IsSubagent = false, want true", ev.EventType)
		}
		if s.Entrypoint != "cli" {
			t.Errorf("%s: Entrypoint = %q", ev.EventType, s.Entrypoint)
		}
	}
}

// TestEventsForLeavesAttributionAbsentWhenTheBlockWasAbsent is the opted-out
// case at the Span layer: a Captured with no Attribution sets none of the
// four fields. IsSubagent in particular must land on its zero value (false)
// rather than some other sentinel, because false IS this type's "absent".
func TestEventsForLeavesAttributionAbsentWhenTheBlockWasAbsent(t *testing.T) {
	pair := mustPair(LaneGateway, sampleIdentity(), "req-noattr", sampleAt, sampleCaptured())
	for _, ev := range pair {
		s := ev.Span
		if s.PromptID != "" || s.PreviousRequestID != "" || s.Entrypoint != "" || s.IsSubagent {
			t.Errorf("%s: span carries attribution %+v with no Attribution on Captured", ev.EventType, s)
		}
	}
}

// TestProviderRequestClassCarriesAttributionToo: a ClassUnknown row (a
// provider path this client has never heard of) still gets the four fields.
// span() must copy them unconditionally rather than only for ClassCompletion,
// because CarriesContent() -- and therefore attribution's relevance -- is
// true for ClassUnknown as well as ClassCompletion.
func TestProviderRequestClassCarriesAttributionToo(t *testing.T) {
	c := sampleCaptured()
	c.HTTPURL = "https://api.anthropic.com/v1/some-new-endpoint" // ClassUnknown
	c.Attribution = map[string]string{"prompt_id": "018f1a2b-0000-7000-8000-000000000005"}
	ev := mustEvent(LaneGateway, sampleIdentity(), "req-unknown", sampleAt, c)
	if ev.ActivityType != client.ActivityTypeProviderRequest {
		t.Fatalf("fixture is not exercising ClassUnknown: ActivityType = %q", ev.ActivityType)
	}
	if ev.Span.PromptID != "018f1a2b-0000-7000-8000-000000000005" {
		t.Errorf("PromptID = %q on a provider_request row, want it carried through", ev.Span.PromptID)
	}
}
