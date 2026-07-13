package client

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// source tags developer-runtime traffic on the wire. core's `source` field is
// free-form/unvalidated (MAPPING.md §6); this distinguishes developer events
// from the SDK's "workflow-telemetry".
const source = "developer-runtime"

// governanceEventPayload mirrors the subset of openbox-core's
// GovernanceEventPayload (internal/content/governance.go:186) that the
// developer-runtime client sets. Fields core populates for Temporal events
// (activity/signal/workflow-specific) are intentionally omitted — they stay
// absent (omitempty), which is additive and INV-8-safe.
type governanceEventPayload struct {
	Source    string `json:"source"`
	EventType string `json:"event_type"`
	// ActivityType is core's pass-through activity_type column (verified stored
	// verbatim for any accepted event_type — openbox-core storage_event.go), which
	// the openbox-fe dashboard's "Activity" column reads first. Always set (see
	// activityLabel) so the UI never falls back to "Unknown".
	ActivityType string          `json:"activity_type,omitempty"`
	WorkflowID   string          `json:"workflow_id"`
	RunID        string          `json:"run_id"`
	Timestamp    string          `json:"timestamp"`
	SpanCount    int             `json:"span_count,omitempty"`
	Spans        []spanData      `json:"spans,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
}

// spanData mirrors the subset of openbox-core's SpanData
// (governance.go:266) an adapter/client SETS. Note the wire tags that differ
// from the field names: FuncName→"function", SpanID→"span_id". Times are int64
// epoch NANOSECONDS (core's OTel convention, verified).
type spanData struct {
	SpanID       string         `json:"span_id"`
	TraceID      string         `json:"trace_id"`
	Name         string         `json:"name"`
	StartTime    int64          `json:"start_time"`
	EndTime      int64          `json:"end_time"`
	Stage        string         `json:"stage,omitempty"`
	SemanticType string         `json:"semantic_type,omitempty"`
	Attributes   map[string]any `json:"attributes,omitempty"`
	FilePath     *string        `json:"file_path,omitempty"`
	FileOp       *string        `json:"file_operation,omitempty"`
	BytesRead    *int64         `json:"bytes_read,omitempty"`
	BytesWritten *int64         `json:"bytes_written,omitempty"`
	LinesCount   *int           `json:"lines_count,omitempty"`
	FuncName     *string        `json:"function,omitempty"`
	Module       *string        `json:"module,omitempty"`
	RequestBody  *string        `json:"request_body,omitempty"`
	ResponseBody *string        `json:"response_body,omitempty"`
}

// buildPayload maps a normalized DevEvent onto core's GovernanceEventPayload
// (MAPPING.md §1-3) and marshals it to the exact bytes that will be signed and
// transmitted. Content-stripping (INV-2) has already run in Emit when
// content-capture is disabled, so any content still present here is authorized.
func buildPayload(ev DevEvent) ([]byte, error) {
	workflowID := ev.WorkspaceID
	if workflowID == "" {
		workflowID = ev.DeveloperDID // stable per-developer identity fallback
	}

	p := governanceEventPayload{
		Source:       source,
		EventType:    string(ev.EventType),
		ActivityType: activityLabel(ev),
		WorkflowID:   workflowID,
		RunID:        ev.SessionID,
		Timestamp:    ev.Timestamp,
	}

	if sp := buildSpan(ev); sp != nil {
		p.Spans = []spanData{*sp}
		p.SpanCount = 1
	}

	meta, err := buildMetadata(ev)
	if err != nil {
		return nil, err
	}
	p.Metadata = meta

	// Compact JSON, matching the reference SDK's serialize_body: the bytes
	// returned here are BOTH hashed for the signature AND sent as the body, so
	// they must be produced exactly once (client.go never re-marshals).
	return json.Marshal(p)
}

// activityLabel resolves the human-readable action label emitted as core's
// pass-through `activity_type` column (openbox-fe's "Activity" column reads it
// first and shows the literal "Unknown" when absent — verified verify/
// trust-tab.tsx). It is derived ONLY from fields that survive the adapter's spool
// round-trip (EventType + Tool.Name are persisted; a `json:"-"` field would not),
// so a spooled tool call still lands its specific tool name:
//   - a tool event (ToolCall/ToolResult) → the specific tool name ("Edit"/
//     "Bash"/"mcp__…"), the most useful Activity label;
//   - everything else (lifecycle, Deploy) → the event_type string.
//
// Always non-empty (EventType always set). Identifier-class only — a tool name or
// an event type — never content (INV-2).
func activityLabel(ev DevEvent) string {
	if ev.EventType == EventToolCall || ev.EventType == EventToolResult {
		if ev.Tool.Name != "" {
			return ev.Tool.Name
		}
	}
	return string(ev.EventType)
}

// buildSpan produces the single carried span, or nil when the event has none.
// It fills the transport fields (span_id/trace_id/name/start/end) the
// tool-agnostic contract omits (MAPPING.md §3).
func buildSpan(ev DevEvent) *spanData {
	s := ev.Span
	if s == nil {
		return nil
	}
	start := rfc3339Nanos(firstNonEmpty(ev.StartedAt, ev.Timestamp))
	end := rfc3339Nanos(firstNonEmpty(ev.EndedAt, ev.Timestamp))

	name, attrs := classificationHints(ev)
	sd := spanData{
		SpanID:    randomID(),
		TraceID:   randomID(),
		Name:      name,
		StartTime: start,
		EndTime:   end,
		Stage:     s.Stage,
		// SemanticType is ADVISORY only: core recomputes it unconditionally at
		// ingest from Name + Attributes (governance_workflow.go:309 →
		// ComputeSemanticTypeFromSpan), so the value we send is discarded. We
		// carry it as intent/forward-compat; classificationHints sets the fields
		// core actually reads so the intended type lands.
		SemanticType: s.SemanticType,
		Attributes:   attrs,
		FilePath:     strPtr(s.FilePath),
		FileOp:       strPtr(s.FileOp),
		BytesRead:    int64Ptr(s.BytesRead),
		BytesWritten: int64Ptr(s.BytesWritten),
		LinesCount:   s.LinesCount,
		FuncName:     strPtr(s.Function),
		Module:       strPtr(s.Module),
		RequestBody:  strPtr(s.RequestBody),
		ResponseBody: strPtr(s.ResponseBody),
	}
	return &sd
}

// fileSpanName maps a file semantic_type to the exact span Name core's
// classifier fallback matches (session.go:257-268) to store that file_* type.
var fileSpanName = map[string]string{
	"file_read":   "file.read",
	"file_write":  "file.write",
	"file_open":   "file.open",
	"file_delete": "file.delete",
}

// classificationHints returns the (span Name, attributes) that make openbox-core
// store the intended semantic_type. Verified against core (session.go:202-272 +
// governance_workflow.go:309): the classifier reads ONLY span.Attributes
// (keys mcp.method / http.method / db.system / file.path) and the span Name —
// it ignores the inbound semantic_type, hook_type, file_operation, and function
// fields entirely. So:
//   - file ops → Name = "file.write"/"file.read"/… + a non-nil file_path
//     (set on the span) hits the fallback file branch → the correct file_* type.
//   - MCP calls → attributes["mcp.method"] = "callTool" → mcp_tool_call.
//   - everything else → the human tool name; core resolves it to "internal"
//     (correct for shell/command tools; the true tool name rides in metadata).
func classificationHints(ev DevEvent) (name string, attrs map[string]any) {
	sem := ""
	if ev.Span != nil {
		sem = ev.Span.SemanticType
	}
	if n, ok := fileSpanName[sem]; ok {
		return n, nil
	}
	if sem == "mcp_tool_call" {
		return firstNonEmpty(ev.Tool.Name, string(ev.EventType)), map[string]any{"mcp.method": "callTool"}
	}
	return firstNonEmpty(ev.Tool.Name, string(ev.EventType)), nil
}

// buildMetadata merges the caller's per-type metadata with the finops keys
// (tokens/cost), the true tool name, and the idempotency key (MAPPING.md §1).
// Never carries content (INV-2) or credentials (INV-1) — those are excluded by
// construction.
//
// event_id goes here deliberately (INV-5): core has no first-class event_id
// field and, verified live, does NOT dedupe the developer event types today, so
// carrying the key in metadata is the only way it reaches the wire for
// server-side dedupe once EXT-core implements it. Within a single Emit the
// retried body is byte-identical, so the id is stable across attempts.
func buildMetadata(ev DevEvent) (json.RawMessage, error) {
	m := make(map[string]any, len(ev.Metadata)+4)
	for k, v := range ev.Metadata {
		m[k] = v
	}
	m["event_id"] = ev.EventID
	// Preserve the real tool name: the span Name is repurposed to drive core's
	// server-side classification (classificationHints), so tool identity would
	// otherwise be lost.
	if ev.Tool.Name != "" {
		m["tool_name"] = ev.Tool.Name
	}
	if ev.Tokens != nil {
		m["tokens"] = ev.Tokens
	}
	if ev.Cost != nil {
		m["cost"] = ev.Cost
	}
	return json.Marshal(m)
}

// stripContent returns a copy of ev with every gated content field removed
// (INV-2/OD4). The caller's event is never mutated. This is the default path
// (content-capture disabled).
func stripContent(ev DevEvent) DevEvent {
	ev.Content = nil
	if ev.Span != nil {
		s := *ev.Span // copy so the caller's Span is untouched
		s.RequestBody = ""
		s.ResponseBody = ""
		ev.Span = &s
	}
	return ev
}

// --- small helpers ---

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// rfc3339Nanos parses an RFC3339 timestamp to epoch nanoseconds, or 0 if empty
// or unparseable (core treats 0 as unset).
func rfc3339Nanos(ts string) int64 {
	if ts == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return 0
	}
	return t.UnixNano()
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "" // never fatal; core generates its own ids if needed
	}
	return hex.EncodeToString(b[:])
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func int64Ptr(p *int) *int64 {
	if p == nil {
		return nil
	}
	v := int64(*p)
	return &v
}
