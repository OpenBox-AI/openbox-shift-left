package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// turnspan.go builds the ONE span a developer session emits. It is not a
// revival of the span layer that decision retired — client/hookspan.go and
// client/spanbuilder.go stay deleted, tool events stay span-less, and nothing
// here mints ids at random or reconstructs a family tuple. It is one carrier,
// with the minimum fields one reader demands.
//
// That reader is openbox-core's goal-alignment extractor, and it accepts
// nothing else. extractAssistantContentFromLatestSpan
// (internal/services/goal_alignment_session.go:64-88) takes the LAST entry of
// payload.Spans and returns "" unless all four hold:
//
// stage == "completed" semantic_type == llm_completion response_body != nil
// response_body unmarshals as {"choices":[{"message":{"content":"…"}}]}
//
// Dev sessions write no spans, so that extractor has always returned "" for
// them and Goal Alignment Trend / Recent Drift have always been empty. This is
// the stopgap that feeds it. openbox-core#130 is the change that retires it by
// teaching the extractor to read the llm_completion activity_output instead.

// wireSpan is the subset of core's SpanData this carrier sets. Field order is
// the wire contract, like every other struct in this package, and the golden
// fixtures pin it.
//
// Absent by construction: parent_span_id, duration_ns, status, events,
// hook_type, request_body, and every family root tuple. They were part of the
// hand-fabricated shape that decision deleted, and nothing reads them.
type wireSpan struct {
	SpanID  string `json:"span_id"`
	TraceID string `json:"trace_id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Stage   string `json:"stage"`
	// StartTime/EndTime are epoch NANOSECONDS — core's OTel convention, not
	// RFC3339 (internal/content/governance.go SpanData).
	StartTime int64 `json:"start_time"`
	EndTime   int64 `json:"end_time"`
	// Attributes carries the classification keys core recomputes semantic_type
	// from, plus the marker that says they are synthesized. See
	// turnSpanAttributes.
	Attributes map[string]any `json:"attributes"`
	// ResponseBody is the assistant turn's text, wrapped in the OpenAI-chat
	// shape the extractor unmarshals. GATED content (INV-2): the caller only
	// reaches this function when content capture survived both gates.
	ResponseBody string `json:"response_body"`
	// SemanticType ships for readability in stored rows and is NEVER relied on:
	// core recomputes it per span before storage
	// (internal/services/governance_workflow.go:303), so whatever is sent here
	// is overwritten by ComputeSemanticTypeFromSpan.
	SemanticType string `json:"semantic_type"`

	// --- Gateway-only fields (that decision, schema 1.5). ---
	//
	// APPENDED, and all omitempty, so the assistant turn span above serializes to
	// byte-identical bytes and its golden fixture does not churn. Key order on the
	// wire is declaration order; inserting any of these earlier would rewrite a
	// pinned fixture for no reason.
	RequestBody     string            `json:"request_body,omitempty"`
	RequestHeaders  map[string]string `json:"request_headers,omitempty"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
	HTTPMethod      string            `json:"http_method,omitempty"`
	HTTPURL         string            `json:"http_url,omitempty"`
	// http_status_CODE, not http_status. Core's SpanData spells it
	// `http_status_code` (internal/content/governance.go), and Go's
	// encoding/json DROPS an unrecognized key silently on Unmarshal — so the
	// shorter spelling reached core's parser and vanished before either policy
	// evaluation or storage saw it, with no error on either side.
	//
	// This is the failure mode this repo's "asserting the struct is not asserting
	// the wire" rule was written for, one level further out: asserting the
	// OUTBOUND bytes is not asserting the RECEIVING TYPE. Every mutation drill and
	// golden fixture here passed while the field was being thrown away.
	HTTPStatus int `json:"http_status_code,omitempty"`
	// CredentialFingerprint is present whether or not content capture is on: it is
	// derived governance evidence, not content. See client.Span for why.
	//
	// Core has NO field for it (verified: zero matches for credential_fingerprint
	// across openbox-core), so this top-level key is dropped on ingest today. It
	// is kept for the day core adds one, and the value ALSO rides
	// attributes["openbox.credential_fingerprint"] — `attributes` is a real
	// SpanData field that survives ingest and is stored, so that is the copy
	// account binding can actually match on. Sending only the top-level key would
	// have made that decision unimplementable while looking finished.
	CredentialFingerprint string `json:"credential_fingerprint,omitempty"`
}

// spanNameLLMCompletion is the span name.
//
// It must not contain "EMBED" or "TOOL" in uppercase: classifyLLMType checks
// those substrings BEFORE falling through to llm_completion
// (internal/content/session.go:323-334), so a name like "LLM_TOOL_TURN" would
// classify as llm_tool_call and the extractor's semantic_type check would fail
// silently. Lowercase "llm_completion" is safe and says what it is.
const spanNameLLMCompletion = "llm_completion"

// spanKindClient mirrors what an outbound model call would be. The span
// describes a request the agent made to the provider, from the client side.
const spanKindClient = "CLIENT"

// synthesizedLLMURL is one of the LLM domains core's isLLMCall matches
// (internal/content/session.go:137-149). See turnSpanAttributes for why a URL
// appears here at all.
const synthesizedLLMURL = "https://api.anthropic.com/v1/messages"

// turnSpanAttributes are the classification keys, and they are the ugliest part
// of this design (OD-0018-1, accepted by the owner 2026-08-13).
//
// The client cannot simply declare semantic_type: core recomputes it. The only
// inputs that yield llm_completion are isLLMCall's — attribute
// http.method == "POST" plus an http.url on core's LLM-domain list
// (internal/content/session.go:451-476). So the span asserts an HTTP request
// this process never made.
//
// openbox.span_synthetic marks that, on every such span, so anyone auditing
// stored spans can tell a described request from an observed one. It is not
// decoration: without it these rows are indistinguishable from real captured
// traffic.
//
// Do NOT delete these keys before openbox-core#130 lands. The failure mode is
// silent — the span still stores, classifies as something else, and the
// extractor's semantic_type check quietly fails with no error anywhere.
func turnSpanAttributes() map[string]any {
	return map[string]any{
		"http.method":            "POST",
		"http.url":               synthesizedLLMURL,
		"openbox.span_synthetic": true,
	}
}

// turnAssistantSpan builds the single span for a TurnCompleted carrying
// assistant text, or nil when there is nothing to carry.
//
// Returning nil for empty text is the whole absence rule: with content capture
// off, Emit's stripContent has already nil'd ev.Content, so this returns nil and
// neither `spans` nor `span_count` appears on the wire at all.
func turnAssistantSpan(ev DevEvent) *wireSpan {
	if ev.Content == nil || ev.Content.Output == "" {
		return nil
	}
	// A turn naming no producer gets no span. turnSpanID hashes the activity id,
	// and turnActivityIDFor returns "" for such an event — so every one of them
	// would share sha256("turnspan\x1f"), one fixed value across all sessions and
	// turns. Core dedupes spans on (span_id, stage), so the second would be
	// absorbed as a duplicate of the first and its assistant text dropped with no
	// error: the silent collision the producer namespaces exist to prevent,
	// through the one path that does not consult them. The event already carries
	// no activity_id, so the span would address nothing in any case.
	if turnActivityIDFor(ev) == "" {
		return nil
	}
	// Cap the TEXT before wrapping it, so the 64KB limit bounds the assistant's
	// words rather than the JSON envelope around them. Capping after would let
	// the wrapper's own bytes eat into the budget and, worse, could truncate
	// mid-escape and produce a response_body core cannot unmarshal — which it
	// reports by logging and returning "", i.e. as silence.
	body, err := json.Marshal(map[string]any{
		"choices": []any{
			map[string]any{"message": map[string]any{"content": capBody(ev.Content.Output)}},
		},
	})
	if err != nil {
		return nil
	}

	// The turn's own window. Falls back to the event timestamp when the
	// transcript gave no real open time — the same rule durationMs follows.
	start := rfc3339Nanos(firstNonEmpty(ev.StartedAt, ev.Timestamp))
	end := rfc3339Nanos(firstNonEmpty(ev.EndedAt, ev.Timestamp))
	if end < start {
		end = start
	}

	return &wireSpan{
		SpanID:       turnSpanID(ev),
		TraceID:      turnTraceID(ev),
		Name:         spanNameLLMCompletion,
		Kind:         spanKindClient,
		Stage:        "completed",
		StartTime:    start,
		EndTime:      end,
		Attributes:   turnSpanAttributes(),
		ResponseBody: string(body),
		SemanticType: activityTypeLLMCompletion,
	}
}

// turnSpanID and turnTraceID derive the ids by HASH rather than at random, and
// that is load-bearing rather than tidy.
//
// Core dedupes spans on (span_id, stage) scoped by session
// (internal/content/governance.go:257-258), and the turn cursor deliberately
// re-reads a window after a crash — it over-reports into a server that
// deduplicates rather than losing a turn's tokens. Random ids would convert that
// safe direction into a second stored span row per re-reported turn, with the
// assistant's text stored twice.
//
// Both derive from fields that survive the spool round-trip, because a flush can
// happen long after the hook process that built the event exited. The span id
// keys on the activity id (per turn, per agent); the trace id on the session, so
// one session's turns share a trace.
//
// 16/32 hex characters — core's OTel id widths.
func turnSpanID(ev DevEvent) string {
	sum := sha256.Sum256([]byte("turnspan\x1f" + turnActivityIDFor(ev)))
	return hex.EncodeToString(sum[:])[:16]
}

func turnTraceID(ev DevEvent) string {
	sum := sha256.Sum256([]byte("turntrace\x1f" + ev.SessionID))
	return hex.EncodeToString(sum[:])[:32]
}
