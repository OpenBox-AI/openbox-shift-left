package client

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"unicode/utf8"
)

// gatewayspan.go builds the span a LOCAL GATEWAY turn carries.
//
// It is a second span producer alongside turnspan.go, and the two are kept apart
// on purpose:
//
// - turnspan.go carries the assistant's REPLY, for exactly one reader — core's
// goal-alignment extractor, which takes the LAST entry of payload.Spans and reads
// response_body as the assistant's words. Nothing else may look like that to it.
// - this file carries an OBSERVED HTTP exchange: the real headers, the real
// bodies, a real status. Its response_body is the provider's actual response, not
// a synthesized chat wrapper.
//
// Both cannot ride one event, which is why a gateway turn is its own activity in
// its own id namespace (see turnActivityIDFor). If they shared an activity_id,
// core's dedupe — (agent_id, workflow_id, run_id, activity_id, event_type) —
// would absorb one as a duplicate of the other and half the evidence would vanish
// with no error anywhere.

// gatewaySpanAttributes carries the classification keys, and they are NOT
// decorative.
//
// Core recomputes semantic_type per span before storage, and isLLMCall is the
// only path to llm_completion: it reads attributes["http.method"] and looks for
// an LLM domain in attributes["http.url"]. A gateway span without these still
// stores — it just classifies as something else, and every reader that filters on
// llm_completion goes quiet. That is the same silent-failure shape that
// decision's synthesized attributes documented, so the keys are set from OBSERVED
// values here rather than fabricated.
//
// Note what is absent: the "synthesized" marker that decision added. This span is
// genuinely observed — a real request, a real response — so claiming otherwise
// would be false, and the marker exists to flag the hook path's fabrication.
// observedSpanAttributes builds the classification keys, and marks the ones that
// were not actually observed.
//
// The in-path lanes — the gateway and the transport relay — SAW the method, URL
// and status they report. The telemetry lane did not: Claude Code's export
// carries neither a method nor a URL, so this client synthesizes both to reach
// core's isLLMCall (the only path to an llm_completion classification). That is a
// described request, not an observed one, and `openbox.span_synthetic` is what
// keeps the two distinguishable in stored rows — the same marker and the same
// reason as turnSpanAttributes.
//
// It is decided from the LANE rather than passed in by the caller, deliberately.
// A mapper that had to remember to set it would eventually forget, and the
// failure is invisible: the span stores either way, and an unmarked synthetic
// span is indistinguishable from real captured traffic. A governance product
// asserting it observed a request it inferred is the overstatement this product
// exists to prevent.
func observedSpanAttributes(lane string, s *Span) map[string]any {
	attrs := map[string]any{}
	if lane == "otel" {
		attrs["openbox.span_synthetic"] = true
	}
	if s.HTTPMethod != "" {
		attrs["http.method"] = s.HTTPMethod
	}
	if s.HTTPURL != "" {
		attrs["http.url"] = s.HTTPURL
	}
	if s.HTTPStatus != 0 {
		attrs["http.status_code"] = s.HTTPStatus
	}
	// The fingerprint's ONLY route into core. Core's SpanData has no
	// credential_fingerprint field, and an unrecognized key is dropped silently on
	// Unmarshal — so without this, account binding had no evidence to match on and
	// could never have fired. `attributes` is carried and stored, so it is where
	// derived evidence has to live until core grows the field.
	//
	// Namespaced `openbox.` so it cannot collide with an OTel convention.
	if s.CredentialFingerprint != "" {
		attrs["openbox.credential_fingerprint"] = s.CredentialFingerprint
	}
	return attrs
}

// Header bounds. The two bodies beside these go through capBody; the header maps
// went to the wire uncapped, which is the one content-bearing field on this span
// that had no bound at all.
//
// Neither limit is theoretical. An inbound header block is bounded by net/http's
// MaxHeaderBytes (1 MiB by default) and a RESPONSE header block by the
// Transport's MaxResponseHeaderBytes, whose default is 10 MiB — so an upstream,
// or anything reaching the gateway's unauthenticated loopback listener, could put
// megabytes of headers on an event that shift-left then SIGNS and POSTs. The
// failure is not a slow request: core rejects an oversized body, so the whole
// event is lost, and the evidence a refusal produced is exactly the evidence an
// auditor needs.
const (
	// maxHeaderValueBytes is generous against real traffic — a model call's
	// largest header is a user-agent or a beta list, both far under this.
	maxHeaderValueBytes = 4096
	// maxHeaderCount bounds the map. A real Anthropic exchange carries ~15.
	maxHeaderCount = 64
)

// capHeaders bounds a captured header map for egress.
//
// Keys are sorted before truncating, and that is load-bearing rather than tidy:
// Go randomizes map iteration, so dropping "whatever came last" would make two
// emissions of the SAME exchange produce different signed bytes. Gateway spans
// are deliberately re-emittable — observedSpanID mints a stable id so a re-emit
// after a crash dedupes instead of storing twice — and evidence that changes
// shape per attempt is evidence an auditor cannot reconcile.
//
// Truncation is marked, not silent: a reader has to be able to tell a short
// header from a shortened one.
func capHeaders(h map[string]string) map[string]string {
	if len(h) == 0 {
		return nil
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > maxHeaderCount {
		keys = keys[:maxHeaderCount]
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = capHeaderValue(h[k])
	}
	return out
}

// capHeaderValue bounds one header value in BYTES, cut on a rune boundary.
//
// Bytes because that is what the bound is FOR: the reason capHeaders exists is
// that core rejects an oversized body and loses the whole event, and core
// measures the body in bytes. The rune boundary is only about not cutting a
// character in half — it is not the unit of the budget.
//
// Those two were conflated, and the result was a bound that could not hold: the
// test was `len(v) > max` (bytes) and the cut was `len([]rune(v)) > max` (runes),
// so a value between the two — 4097 bytes of CJK is ~1366 runes — entered the
// branch and then declined to truncate, reaching the wire uncapped. Worst case
// was 4 bytes per rune × 4096 runes = 16 KiB per value and ~1 MiB per map,
// exactly the loss the bound exists to prevent. TestGatewayHeaderCapIsAByteBound
// drives that gap; the old shape passes every other case in the file.
func capHeaderValue(v string) string {
	if len(v) <= maxHeaderValueBytes {
		return v
	}
	end := maxHeaderValueBytes
	for end > 0 && !utf8.RuneStart(v[end]) {
		end--
	}
	return v[:end] + "…[truncated]"
}

// observedLane names the producer that OBSERVED this turn, and its request id.
//
// The precedence is turnActivityIDFor's, deliberately and not coincidentally:
// proxy, then gateway, then otel (that decision — in-path relay outranks a
// client-asserted lane). If the two disagreed, an event could take its
// activity_id from one lane and its span id from another, and core would file
// half the evidence under a row the other half never joins.
//
// A well-formed event carries exactly one (the contract's turnProducer oneOf
// rejects two), so the order only decides how a MALFORMED event is attributed.
func observedLane(ev DevEvent) (name, id string) {
	switch {
	case ev.ProxyRequestID != "":
		return "proxy", ev.ProxyRequestID
	case ev.GatewayRequestID != "":
		return "gw", ev.GatewayRequestID
	case ev.OtelRequestID != "":
		return "otel", ev.OtelRequestID
	}
	return "", ""
}

// observedSpanID derives the span id from the observing lane's request id, so a
// re-emit after a crash mints the same id and core's span dedupe — (span_id,
// stage) scoped by session_id — absorbs it instead of storing a second row. The
// same over-report-rather-than-lose direction the turn cursor already takes.
//
// The lane name appears TWICE — in the hash input and in the prefix — for the
// reason the activity_id namespaces exist: two lanes describing the same turn
// must not mint the same span id, or core's dedupe absorbs one and half the
// evidence vanishes with no error.
//
// The two are redundant with each other, and that is measured rather than
// assumed: deleting either one alone leaves the ids disjoint (the prefix keeps
// them distinct strings; the hash keeps the digests distinct), and only deleting
// BOTH collides. So TestObservingLaneSpanIDsAreDisjoint goes red on the
// both-removed mutation and green on each single one — stated here because a
// comment claiming either is individually load-bearing would be false, and this
// one said exactly that until the drill was run.
//
// The gateway's derivation is byte-identical to what it was before the other
// lanes existed ("gwspan" / "gw-"), so no stored id moves.
func observedSpanID(ev DevEvent) string {
	lane, id := observedLane(ev)
	if lane == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(lane + "span\x1f" + ev.SessionID + "\x1f" + id))
	return lane + "-" + hex.EncodeToString(sum[:16])
}

// observedSpan builds the span for a model call one of the OBSERVING lanes saw —
// the local gateway, the local transport relay, or the local telemetry receiver.
//
// Returns nil when the event is not one of those turns or carries no observed
// evidence, so a hook-only install emits exactly what it emitted before.
//
// It was gateway-only, and gating on GatewayRequestID alone was a latent defect
// the moment that decision declared the other two discriminators: an event
// carrying OtelRequestID or ProxyRequestID plus a populated Span was accepted,
// spooled, signed and POSTed with NO span attached. That failure is this repo's
// signature shape — a working-looking lane carrying none of its evidence — and
// it is exactly what deleting the http.* keys would also cause.
func observedSpan(ev DevEvent) *wireSpan {
	lane, _ := observedLane(ev)
	if lane == "" || ev.Span == nil {
		return nil
	}
	s := ev.Span
	// Nothing observed means nothing to say. The fingerprint alone is enough to
	// justify a span: it is the account-binding evidence, and a call whose
	// content was stripped by the gate still needs to be attributable.
	if s.HTTPMethod == "" && s.HTTPURL == "" && s.CredentialFingerprint == "" {
		return nil
	}

	// The call's own window, with the same fallback rule durationMs and the
	// assistant span already follow.
	start := rfc3339Nanos(firstNonEmpty(ev.StartedAt, ev.Timestamp))
	end := rfc3339Nanos(firstNonEmpty(ev.EndedAt, ev.Timestamp))
	if end < start {
		end = start
	}

	return &wireSpan{
		SpanID:    observedSpanID(ev),
		TraceID:   turnTraceID(ev),
		Name:      spanNameLLMCompletion,
		Kind:      spanKindClient,
		Stage:     "completed",
		StartTime: start,
		EndTime:   end,
		// Content, and already gated: Emit's stripContent ran before this, so a
		// body or header map still present here is authorized. Capped for the
		// same reason every other content field is — the signed bytes are these
		// bytes.
		ResponseBody:    capBody(s.ResponseBody),
		RequestBody:     capBody(s.RequestBody),
		RequestHeaders:  capHeaders(s.RequestHeaders),
		ResponseHeaders: capHeaders(s.ResponseHeaders),
		Attributes:      observedSpanAttributes(lane, s),
		// Structural / derived: these survive the gate by design.
		HTTPMethod:            s.HTTPMethod,
		HTTPURL:               s.HTTPURL,
		HTTPStatus:            s.HTTPStatus,
		CredentialFingerprint: s.CredentialFingerprint,
		SemanticType:          activityTypeLLMCompletion,
	}
}
