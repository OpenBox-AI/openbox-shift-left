package gatewayemit

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// attributionMarker opens Claude Code's own per-call correlation block. It
// reads like an HTTP header name, but it never rides one: it is a system[]
// TEXT ELEMENT inside the request body, and the whole block is absent when
// the developer opts out via CLAUDE_CODE_ATTRIBUTION_HEADER -- so its absence
// here means UNCLASSIFIED, never "background" or "sidecar".
const attributionMarker = "x-anthropic-billing-header:"

// maxAttributionBlockBytes bounds how far past the marker this parser looks
// for the run of ';'-separated tokens, mirroring modelBudget's reasoning
// (internal/gateway/requestselect.go): the real block is a handful of short
// entries, so this is far above any honest one and far below anything that
// could make a hostile body cost real work. It also gives the extraction
// somewhere to stop when the JSON string carrying the block never closes
// within the window -- a truncated or otherwise hostile body.
const maxAttributionBlockBytes = 4096

// maxAttributionValueBytes bounds ONE token's value once split out, backing
// off to the nearest valid rune boundary. Mirrors
// telemetryemit.capIdentifier's 256-byte bound (unreachable from this
// package: gatewayemit imports no telemetry code) for the same reason -- the
// values here are the same class of vendor identifier, and they arrive from a
// body an attacker controls.
const maxAttributionValueBytes = 256

// attributionTokenKeys maps the token name Claude Code writes to the metadata
// key this lane promotes it under. cc_workload and cc_version are
// deliberately absent: this phase does not bind either.
var attributionTokenKeys = map[string]string{
	"cc_prompt_id":  "prompt_id",
	"cc_prev_req":   "previous_request_id",
	"cc_entrypoint": "entrypoint",
}

// subagentSourceKey/subagentMetadataKey are handled outside
// attributionTokenKeys because the rule is different in kind, not just in
// name: the wire key is emitted only when the source entry is present AND
// exactly "true", never "false" and never for any other value.
const (
	subagentSourceKey   = "cc_is_subagent"
	subagentMetadataKey = "is_subagent"
)

// ParseRequestAttribution reads Claude Code's per-call correlation block out
// of a raw relayed request body and promotes four of its tokens. It never
// errors and never panics: it sits in the request path of a relay whose one
// hard rule is that it must not break the tool, so a non-JSON, truncated, or
// unrecognised body degrades to an empty map rather than a partial guess.
//
// The block legitimately lives in exactly one place: a system[] text
// element. requestselect.go's own measurement is why that matters here --
// `messages` precedes `system` in most real bodies, and a messages[] entry is
// conversation content an attacker, a pasted file, or the model's own reply
// controls. This function locates the top-level `system` field with
// encoding/json BEFORE it ever looks for the marker, so messages[] -- and
// every other top-level key -- is structurally never a source, not merely a
// source this scan happens not to reach first.
//
// Locating `system` is the only role the JSON parse plays. What it finds
// inside is read the same way it always was: a plain substring scan, not a
// further JSON parse -- tolerant by design, never a schema, the same rule
// internal/gateway's own request-body selector states for this field. Any
// shape besides "a top-level JSON object naming `system`" -- a non-object
// body, a missing or null `system`, a truncated document -- degrades to the
// empty map through the one exit every other unrecognised shape already
// uses.
func ParseRequestAttribution(raw []byte) map[string]string {
	out := map[string]string{}
	system := systemFieldRaw(raw)
	markerAt := bytes.Index(system, []byte(attributionMarker))
	if markerAt < 0 {
		return out
	}
	block := attributionBlock(system[markerAt+len(attributionMarker):])
	for _, tok := range completeAttributionTokens(block) {
		key, value, ok := splitAttributionToken(tok)
		if !ok {
			continue
		}
		if key == subagentSourceKey {
			if value == "true" {
				out[subagentMetadataKey] = "true"
			}
			continue
		}
		wireKey, known := attributionTokenKeys[key]
		if !known || value == "" {
			continue
		}
		out[wireKey] = boundAttributionValue(value)
	}
	return out
}

// systemFieldRaw returns the raw JSON bytes of the top-level `system` field,
// whatever shape it is -- array, string, object, or absent -- so the marker
// search has a haystack that structurally excludes messages[] and every
// other key. nil for anything that is not a JSON object at the top level;
// the caller reads that the same as "no system key at all".
//
// json.Unmarshal is used only to find the FIELD, never to validate the
// block's own shape: a json.RawMessage target accepts any JSON value without
// caring whether it is the array Claude Code sends or something else
// entirely, which keeps this anchoring as tolerant of an unexpected `system`
// shape as the substring scan it feeds always was. It costs one bounded pass
// over `raw` and one allocation sized to `system`'s own bytes, not the whole
// body -- the same shape internal/gateway/requestselect.go's own selector
// already pays on this exact request path to find `messages`.
func systemFieldRaw(raw []byte) []byte {
	var fields struct {
		System json.RawMessage `json:"system"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	return fields.System
}

// attributionBlock returns the bytes between the marker and either the first
// UNESCAPED double quote -- the JSON string carrying the block closing -- or
// maxAttributionBlockBytes, whichever comes first. Bounding here rather than
// trusting the closing quote is what keeps a body that never closes the
// string, truncated or simply hostile, from costing more than a fixed amount
// of work.
//
// A quote is unescaped when it is preceded by an EVEN number of consecutive
// '\' bytes, zero included: each adjacent pair of backslashes is one escaped
// backslash, so an even run leaves the quote itself bare, and an odd run
// means the last '\' escapes it. Sampling one byte back gets this wrong for
// any run of two or more -- a value ending "...\\" reads its own real closing
// quote as escaped and scans on past it into whatever follows -- which is why
// this counts the run instead.
func attributionBlock(after []byte) string {
	end := len(after)
	if end > maxAttributionBlockBytes {
		end = maxAttributionBlockBytes
	}
	window := after[:end]
	for i, b := range window {
		if b != '"' {
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0 && window[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			window = window[:i]
			break
		}
	}
	return string(window)
}

// completeAttributionTokens splits on ';' and drops the LAST piece
// unconditionally. The observed shape terminates every token, including the
// final one, with its own ';', so the piece after the last real ';' is
// either empty (the well-formed case) or a partial fragment with no
// terminator of its own -- a truncated body, or trailing JSON syntax
// attributionBlock's window did not manage to cut before. Dropping it
// unconditionally means a partial fragment is never mistaken for a complete
// token.
func completeAttributionTokens(block string) []string {
	parts := strings.Split(block, ";")
	return parts[:len(parts)-1]
}

// splitAttributionToken trims one ';'-delimited piece and splits it on the
// first '='; ok is false for anything with no '=' at all.
func splitAttributionToken(tok string) (key, value string, ok bool) {
	tok = strings.TrimSpace(tok)
	k, v, found := strings.Cut(tok, "=")
	if !found {
		return "", "", false
	}
	return strings.TrimSpace(k), strings.TrimSpace(v), true
}

// boundAttributionValue caps a token's value so one oversized field cannot
// make an identifier unbounded, backing the cut off to the last complete rune
// rather than leaving a split multi-byte sequence in the output.
//
// strings.ToValidUTF8 was tried here first and is the wrong tool for this
// job: given invalid UTF-8 anywhere in the first maxAttributionValueBytes
// bytes -- not only at the cut point itself -- it DELETES that byte (or run)
// and keeps going, closing the gap rather than stopping there. That silently
// edits the interior of a value this function's contract is only ever to
// bound the LENGTH of. trimTrailingPartialRune only ever removes bytes from
// the END, so a byte before the cut, valid or not, is never touched.
func boundAttributionValue(s string) string {
	if len(s) <= maxAttributionValueBytes {
		return s
	}
	return trimTrailingPartialRune(s[:maxAttributionValueBytes])
}

// trimTrailingPartialRune backs a byte-truncated string off one byte at a
// time until it ends on a complete rune or is empty, so a cut that landed
// inside a multi-byte sequence loses only that incomplete tail.
func trimTrailingPartialRune(s string) string {
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size > 1 {
			return s
		}
		s = s[:len(s)-1]
	}
	return s
}
