package client

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// This file is the E7-S3 flat-SpanData BUILDER: the primitive that turns an
// adapter's knowledge of a tool call into the flat Core SpanData map the base
// SDK puts on the wire — the shape client/hookspan.go's AssertHookWireShape (the
// E7-S1 conformance gate) checks. It is byte-faithful to the base SDK's
// construction in
//   openbox-sdk-python/openbox_core/contracts/otel_spans.py  (from_otel_span)
//   openbox-sdk-python/openbox_core/otel/trace_context.py    (format_span_id/format_trace_id)
// re-expressed in Go with no OpenTelemetry dependency (the wire is flat
// SpanData, not OTel — build the dict directly, per migration-doc §3).
//
// Scope boundary: this story delivers the builder + trace/parent-linkage
// machinery + the wire-shape conformance test only. The event→wire MAPPING
// (which DevEvent becomes ActivityStarted vs ActivityCompleted vs Workflow*/
// Signal, and the content-gating/truncation of shell_command & request/response
// bodies) is E7-S4/E7-S5; those stories rewire buildPayload/the adapter onto
// this primitive and retire the hand-built client/event.go Span. Nothing here
// gates content: the builder places whatever family/body fields the caller
// hands it (post content-gate) at the span ROOT only, never in attributes or
// metadata (INV-2 — content lives only in gated span bodies).

// TraceContext holds a developer session's stable 32-hex trace_id and mints
// fresh 16-hex span ids under it. One session (one Core run) owns one
// TraceContext so every span it emits shares the trace and correlates. It
// carries no content and is safe to reuse across a session's events.
//
// Pairing a ToolResult's completed span to its ToolCall's started span follows
// the base SDK, where both stages are the SAME OTel span: the emitter reuses
// the started span's id as the completed HookSpan.SpanID (same span_id/trace_id/
// parent_span_id, differing only in stage/end_time/duration_ns — verified
// against openbox_core/hooks/wrappers.py, which passes one span object to both
// the started and completed hooks). ParentSpanID is the distinct case of
// nesting a span under an enclosing activity/root span. The event→wire pairing
// itself is wired in E7-S4; this primitive supplies both mechanisms.
type TraceContext struct {
	traceID string // 32 lowercase hex, stable for the session
}

// NewTraceContext mints a session TraceContext with a fresh random 32-hex
// trace_id.
func NewTraceContext() *TraceContext {
	return &TraceContext{traceID: newHexID(traceIDBytes)}
}

// TraceContextFrom rebuilds a TraceContext from a persisted 32-hex trace_id, so
// a spooled/rehydrated session keeps its trace correlation across process
// restarts (the SL-4 spool round-trips events). A trace_id that is not 32
// lowercase hex is rejected and a fresh one minted, so the returned context
// always yields wire-valid ids.
func TraceContextFrom(traceID string) *TraceContext {
	if traceIDRe.MatchString(traceID) {
		return &TraceContext{traceID: traceID}
	}
	return NewTraceContext()
}

// TraceID returns the session's stable 32-hex trace_id (e.g. to persist it
// alongside a spooled event so a later flush reuses it).
func (tc *TraceContext) TraceID() string { return tc.traceID }

// NewSpanID mints a fresh 16-hex span id under this trace. The emitter keeps a
// started span's id and reuses it as the paired completed span's SpanID (base
// SDK shared-span pairing; see TraceContext).
func (tc *TraceContext) NewSpanID() string { return newHexID(spanIDBytes) }

// HookSpan is the semantic input to BuildHookSpan: what an adapter knows about
// one tool call, independent of the flat-wire mechanics. The builder fills the
// common root fields, defaults, and the present-but-null family tuple around
// it. Family root values and gated content bodies (file_path, shell_command,
// request_body, …) go in Fields; the builder never invents or relocates them.
type HookSpan struct {
	HookType HookType // file_operation | shell | mcp | tool (client/hookspan.go)
	Name     string   // span name; defaults to the hook_type string when empty
	Kind     string   // OTel kind override; defaults to KindFor(HookType)
	Stage    string   // "started" (ToolCall) | "completed" (ToolResult)

	// SpanID is this span's 16-hex id; minted from the TraceContext when empty.
	SpanID string
	// ParentSpanID links this span to its parent (e.g. a completed span to its
	// started span). Empty ⇒ root span ⇒ parent_span_id:null on the wire.
	ParentSpanID string

	// StartTime/EndTime are epoch NANOSECONDS (Core's OTel convention). For a
	// started span EndTime is ignored and end_time/duration_ns emit as null; for
	// a completed span duration_ns = EndTime-StartTime (clamped to ≥0).
	StartTime int64
	EndTime   int64

	// Attributes carries structural hints Core's classifier reads (e.g.
	// {"mcp.method":"callTool"}). Never content (INV-2). Defaults to {} so the
	// common root field is always present.
	Attributes map[string]any

	// Status mirrors the OTel status dict; defaults to {"code":"UNSET",
	// "description":null} exactly as the base SDK does.
	Status map[string]any

	// Fields are caller-supplied Core root fields merged at the span root: family
	// identifiers (file_path, shell_exit_code, mcp_server, …) and gated content
	// bodies (shell_command, request_body, response_body). Only non-nil entries
	// are written; every remaining field of the hook_type's family tuple is then
	// filled present-but-null. Content appears here and nowhere else (INV-2).
	Fields map[string]any
}

// BuildHookSpan constructs one flat Core SpanData map from a HookSpan, faithful
// to the base SDK's from_otel_span. The result carries every common root field
// (present, null when absent), the full family tuple for HookType (present, null
// when the caller did not supply it), 16/32-hex ids, and started/completed
// staging — so it passes AssertHookWireShape. tc supplies the stable trace_id
// and mints the span_id when HookSpan.SpanID is empty.
func BuildHookSpan(tc *TraceContext, in HookSpan) map[string]any {
	spanID := in.SpanID
	if spanID == "" {
		spanID = tc.NewSpanID()
	}

	name := in.Name
	if name == "" {
		name = string(in.HookType)
	}
	if name == "" {
		name = "span" // mirrors from_otel_span's final fallback
	}

	kind := in.Kind
	if kind == "" {
		kind = KindFor(in.HookType)
	}

	// Shallow-copy the caller's attributes (mirrors from_otel_span's
	// `dict(attributes)`), so a caller mutating its map after the build never
	// mutates the emitted span; default to {} so the common root field is present.
	attrs := make(map[string]any, len(in.Attributes))
	for k, v := range in.Attributes {
		attrs[k] = v
	}
	status := in.Status
	if status == nil {
		status = map[string]any{"code": "UNSET", "description": nil}
	}

	// Staging: a started span emits end_time:null + duration_ns:null; a completed
	// span emits a real end_time and duration_ns = end-start. Kept as untyped any
	// so the started nulls marshal as JSON null.
	//
	// Deliberate deviation from from_otel_span (which emits end-start unclamped):
	// the base SDK reads monotonic OTel clock ints, so start≤end always holds;
	// shift-left derives times from RFC3339 timestamps that can arrive skewed/
	// out-of-order, so we clamp end up to start rather than emit a negative
	// duration. Wire-shape-safe (AssertHookWireShape does not check the sign).
	var endTime, durationNS any
	if in.Stage != "started" {
		end := in.EndTime
		if end < in.StartTime {
			end = in.StartTime
		}
		endTime = end
		durationNS = end - in.StartTime
	}

	span := map[string]any{
		"span_id":        spanID,
		"trace_id":       tc.traceID,
		"parent_span_id": nil,
		"name":           name,
		"kind":           kind,
		"stage":          in.Stage,
		"start_time":     in.StartTime,
		"end_time":       endTime,
		"duration_ns":    durationNS,
		"attributes":     attrs,
		"status":         status,
		"events":         []any{},
		"hook_type":      string(in.HookType),
		"error":          nil,
	}
	if in.ParentSpanID != "" {
		span["parent_span_id"] = in.ParentSpanID
	}

	// Merge caller family/body fields (non-nil overrides), then fill the rest of
	// the family tuple present-but-null — the same order as from_otel_span
	// (fields merge, then _ROOT_FIELDS_BY_HOOK_TYPE setdefault(None)).
	for k, v := range in.Fields {
		if v != nil {
			span[k] = v
		}
	}
	for _, f := range FamilyRootFields[in.HookType] {
		if _, present := span[f]; !present {
			span[f] = nil
		}
	}

	return span
}

// BuildHookEvent wraps flat hook spans in the base SDK's ActivityStarted hook
// event envelope: event_type="ActivityStarted", hook_trigger=true, span_count,
// and spans as []any (so AssertHookWireShape reads it without a JSON round-trip;
// see client/hookspan_test.go L3). activity_id/activity_type are set when
// supplied. This is the envelope SHAPE only — the event→wire mapping (which
// DevEvent becomes this event, and its activity_type/stage) is E7-S4.
func BuildHookEvent(activityID, activityType string, spans ...map[string]any) map[string]any {
	anySpans := make([]any, len(spans))
	for i, s := range spans {
		anySpans[i] = s
	}
	ev := map[string]any{
		"event_type":   "ActivityStarted",
		"hook_trigger": true,
		"span_count":   len(spans),
		"spans":        anySpans,
	}
	if activityID != "" {
		ev["activity_id"] = activityID
	}
	if activityType != "" {
		ev["activity_type"] = activityType
	}
	return ev
}

const (
	spanIDBytes  = 8  // 8 bytes  → 16 lowercase hex
	traceIDBytes = 16 // 16 bytes → 32 lowercase hex
)

// newHexID returns nBytes of randomness as lowercase hex (2*nBytes chars). On
// the (effectively impossible on Linux) crypto/rand failure it returns the
// all-zeros id of the right width — shape-valid hex and exactly the base SDK's
// "0"*16 / "0"*32 sentinel for an unknown id (from_otel_span), so a builder
// never emits a malformed id.
func newHexID(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", nBytes*2)
	}
	return hex.EncodeToString(b)
}
