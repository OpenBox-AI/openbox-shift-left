package client

import (
	"crypto/sha256"
	"encoding/hex"
)

// gatewayspan.go builds the span a LOCAL GATEWAY turn carries (ADR-0021).
//
// It is a second span producer alongside turnspan.go, and the two are kept apart
// on purpose:
//
//   - turnspan.go carries the assistant's REPLY, for exactly one reader — core's
//     goal-alignment extractor, which takes the LAST entry of payload.Spans and
//     reads response_body as the assistant's words. Nothing else may look like
//     that to it.
//   - this file carries an OBSERVED HTTP exchange: the real headers, the real
//     bodies, a real status. Its response_body is the provider's actual response,
//     not a synthesized chat wrapper.
//
// Both cannot ride one event, which is why a gateway turn is its own activity in
// its own id namespace (see turnActivityIDFor). If they shared an activity_id,
// core's dedupe — (agent_id, workflow_id, run_id, activity_id, event_type) —
// would absorb one as a duplicate of the other and half the evidence would
// vanish with no error anywhere.

// gatewaySpanAttributes carries the classification keys, and they are NOT
// decorative.
//
// Core recomputes semantic_type per span before storage, and isLLMCall is the
// only path to llm_completion: it reads attributes["http.method"] and looks for
// an LLM domain in attributes["http.url"]. A gateway span without these still
// stores — it just classifies as something else, and every reader that filters on
// llm_completion goes quiet. That is the same silent-failure shape ADR-0018's
// synthesized attributes documented, so the keys are set from OBSERVED values
// here rather than fabricated.
//
// Note what is absent: the "synthesized" marker ADR-0018 added. This span is
// genuinely observed — a real request, a real response — so claiming otherwise
// would be false, and the marker exists to flag the hook path's fabrication.
func gatewaySpanAttributes(s *Span) map[string]any {
	attrs := map[string]any{}
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
	// Unmarshal — so without this, account binding (ADR-0021 §6) had no evidence
	// to match on and could never have fired. `attributes` is carried and stored,
	// so it is where derived evidence has to live until core grows the field.
	//
	// Namespaced `openbox.` so it cannot collide with an OTel convention.
	if s.CredentialFingerprint != "" {
		attrs["openbox.credential_fingerprint"] = s.CredentialFingerprint
	}
	return attrs
}

// gatewaySpanID derives the span id from the gateway's request id, so a re-emit
// after a crash mints the same id and core's span dedupe — (span_id, stage)
// scoped by session_id — absorbs it instead of storing a second row. The same
// over-report-rather-than-lose direction the turn cursor already takes.
func gatewaySpanID(ev DevEvent) string {
	sum := sha256.Sum256([]byte("gwspan\x1f" + ev.SessionID + "\x1f" + ev.GatewayRequestID))
	return "gw-" + hex.EncodeToString(sum[:16])
}

// gatewayObservedSpan builds the span for a gateway-observed model call.
//
// Returns nil when the event is not a gateway turn or carries no gateway
// evidence, so a hook-only install emits exactly what it emitted before.
func gatewayObservedSpan(ev DevEvent) *wireSpan {
	if ev.GatewayRequestID == "" || ev.Span == nil {
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
		SpanID:    gatewaySpanID(ev),
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
		RequestHeaders:  s.RequestHeaders,
		ResponseHeaders: s.ResponseHeaders,
		Attributes:      gatewaySpanAttributes(s),
		// Structural / derived: these survive the gate by design.
		HTTPMethod:            s.HTTPMethod,
		HTTPURL:               s.HTTPURL,
		HTTPStatus:            s.HTTPStatus,
		CredentialFingerprint: s.CredentialFingerprint,
		SemanticType:          activityTypeLLMCompletion,
	}
}
