package client

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
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

// buildPayload maps a normalized DevEvent onto core's GovernanceEventPayload and
// marshals it to the exact bytes that will be signed and transmitted.
// Content-stripping (INV-2) has already run in Emit when content-capture is
// disabled, so any content still present here is authorized.
//
// E7-S4 splits the wire by event class:
//   - ToolCall/ToolResult serialize onto the base SDK's flat hook shape
//     (ActivityStarted + hook_trigger + a flat SpanData whose `stage` field is
//     the only started-vs-completed distinguisher on the wire — a ToolResult is
//     NOT ActivityCompleted; that value is reserved for hook-less lifecycle
//     events, verified against openbox-sdk-python contracts/events.py
//     `wire_event_type`/`hook` + conformance/fake_core.assert_hook_wire_shape).
//     The started+completed pair share an activity_id and span_id so Core (and
//     the shared dashboard) pair them onto one timeline row.
//   - Everything else keeps the legacy envelope; E7-S5 rewires lifecycle onto
//     Workflow* and prompt/commit/deploy onto SignalReceived.
func buildPayload(ev DevEvent) ([]byte, error) {
	if ev.EventType == EventToolCall || ev.EventType == EventToolResult {
		return buildHookPayload(ev)
	}
	return buildLegacyPayload(ev)
}

// buildLegacyPayload is the pre-E7-S4 envelope path, retained for the lifecycle /
// prompt / commit / deploy events E7-S5 will migrate. It carries at most one
// legacy span; no non-tool event sets ev.Span today, so buildSpan returns nil and
// these events go out span-less.
func buildLegacyPayload(ev DevEvent) ([]byte, error) {
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

// buildHookPayload serializes a ToolCall/ToolResult onto the base SDK's flat
// hook wire shape via the E7-S3 builder (BuildHookSpan/BuildHookEvent) + the
// E7-S1 hook types. The result carries the ActivityStarted+hook envelope
// (event_type/hook_trigger/activity_id/activity_type/span_count/spans) merged
// with the session-attach fields Core needs (source, workflow_id, run_id,
// timestamp, metadata) — exactly as the base SDK's to_payload_dict merges the
// ActivityContext onto the flat top-level dict.
func buildHookPayload(ev DevEvent) ([]byte, error) {
	workflowID := ev.WorkspaceID
	if workflowID == "" {
		workflowID = ev.DeveloperDID // stable per-developer identity fallback
	}

	// One session (one Core run) owns one stable trace. Because hooks fire as
	// separate short-lived processes (and events round-trip the SL-4 spool), there
	// is no shared in-memory TraceContext to thread; instead the trace_id is
	// DERIVED deterministically from the session id, so every span in a session
	// shares it without persisting any state. TraceContextFrom reuses the derived
	// 32-hex id (E7-S3 rehydration path).
	tc := TraceContextFrom(sessionTraceID(ev.SessionID))
	span := buildHookSpan(tc, ev)

	// activity_type = the specific tool name (the dashboard's Activity label);
	// activity_id = the deterministic pairing key shared by this call's started
	// and completed spans.
	body := BuildHookEvent(hookActivityID(ev), activityLabel(ev), span)
	body["source"] = source
	body["workflow_id"] = workflowID
	body["run_id"] = ev.SessionID
	if ev.Timestamp != "" {
		body["timestamp"] = ev.Timestamp
	}

	meta, err := buildMetadata(ev)
	if err != nil {
		return nil, err
	}
	// json.RawMessage marshals as the raw object (not a quoted string) when nested
	// in the map, so metadata emits as a nested object exactly as the struct path.
	body["metadata"] = json.RawMessage(meta)

	// Compact JSON, signed-once (see buildLegacyPayload) — the bytes returned are
	// both hashed for the signature and sent as the body.
	return json.Marshal(body)
}

// buildHookSpan turns a tool DevEvent into one flat Core SpanData via the E7-S3
// builder. The started (ToolCall) and completed (ToolResult) spans of the same
// tool call share a deterministic span_id (base SDK shared-span pairing) and
// carry the family + attribute source fields Core's classifier reads.
//
// Unlike the base SDK — which shares ONE span object across both stages, so
// start_time is identical — shift-left builds the two spans in SEPARATE hook
// processes from SEPARATE events. Claude Code's PostToolUse exposes no start
// time (the mapper sets only EndedAt — mapper.go PostToolUse), so a completed
// span's start_time is its own timestamp and duration_ns is therefore 0. This is
// a known stateless-pairing limitation: the pair shares span_id/trace_id and
// pairs correctly, but the completed span's start_time/duration_ns are not
// derived from the started span (recovering the real start would need
// cross-process state — a PreToolUse→PostToolUse start-time stash — which is out
// of E7-S4's scope; tracked for the E7-S6 dashboard-timeline work). Wire-shape-
// valid regardless (AssertHookWireShape constrains neither the duration sign nor
// non-zero-ness).
func buildHookSpan(tc *TraceContext, ev DevEvent) map[string]any {
	stage := "started"
	if ev.EventType == EventToolResult {
		stage = "completed"
	}
	if ev.Span != nil && ev.Span.Stage != "" {
		stage = ev.Span.Stage // adapter-set stage wins when present
	}

	hookType := hookTypeFor(ev.Tool.Kind)
	name, attrs, fields := hookSpanShape(ev, hookType)

	in := HookSpan{
		HookType: hookType,
		Name:     name,
		Stage:    stage,
		// Deterministic + shared across the started/completed pair (see hookSpanID).
		SpanID:     hookSpanID(ev),
		StartTime:  rfc3339Nanos(firstNonEmpty(ev.StartedAt, ev.Timestamp)),
		EndTime:    rfc3339Nanos(firstNonEmpty(ev.EndedAt, ev.Timestamp)),
		Attributes: attrs,
		Fields:     fields,
	}
	return BuildHookSpan(tc, in)
}

// hookTypeFor maps the SL-1 tool kind (shell|file|mcp) onto the E7-S1 hook type.
// The mapping is 1:1 and provider-agnostic (every adapter's normalized DevEvent
// benefits with no per-adapter code — the CLAUDE.md core/adapter split). The
// generic `tool` hook type covers any future kind outside the SL-1 taxonomy.
func hookTypeFor(k ToolKind) HookType {
	switch k {
	case ToolFile:
		return HookFileOperation
	case ToolMCP:
		return HookMCP
	case ToolShell:
		return HookShell
	default:
		return HookTool
	}
}

// hookSpanShape derives the span name, classifier attributes, and root family/
// body fields for a tool DevEvent. It re-expresses the pre-E7-S4
// classificationHints in the flat-hook world:
//   - file ops → name "file.read"/"file.write"/… + root file_path/file_operation/
//     byte counts, so Core's fallback classifier (span.Name + non-nil file_path)
//     stores the file_* semantic type.
//   - mcp → attributes["mcp.method"]="callTool" (the ONLY key Core reads today for
//     mcp_tool_call) plus the structural mcp.server/mcp.tool hints and the mcp_*
//     root family identifiers (Core reads these first-class after E7-S2).
//   - shell → hook_type=shell only; shell_command is CONTENT and is never carried
//     on the observe/egress path (the command is read solely for the LOCAL enforce
//     decision — INV-2), so it stays present-but-null; shell_exit_code is not
//     exposed by the CC hook payload.
//
// Gated content bodies (request_body/response_body) ride at the span ROOT and
// only when still present (content-capture on; stripContent nulled them otherwise
// — INV-2). They are size-capped to maxBodySize before egress (capBody / G_SEC
// SEC-1), so the opt-in content-capture path cannot ship an unbounded body — the
// same privacy cap the base SDK applies before signing. Every family field the
// caller does not supply is filled present-but-null by BuildHookSpan, so the
// payload passes AssertHookWireShape.
func hookSpanShape(ev DevEvent, ht HookType) (name string, attrs, fields map[string]any) {
	fields = map[string]any{}
	s := ev.Span
	if s != nil {
		if s.RequestBody != "" {
			fields["request_body"] = capBody(s.RequestBody)
		}
		if s.ResponseBody != "" {
			fields["response_body"] = capBody(s.ResponseBody)
		}
	}

	switch ht {
	case HookFileOperation:
		if s != nil {
			if n, ok := fileSpanName[s.SemanticType]; ok {
				name = n
			}
			if s.FilePath != "" {
				fields["file_path"] = s.FilePath
			}
			if s.FileOp != "" {
				fields["file_operation"] = s.FileOp
			}
			if s.BytesRead != nil {
				fields["bytes_read"] = int64(*s.BytesRead)
			}
			if s.BytesWritten != nil {
				fields["bytes_written"] = int64(*s.BytesWritten)
			}
			if s.LinesCount != nil {
				fields["lines_count"] = *s.LinesCount
			}
		}
	case HookMCP:
		attrs = map[string]any{"mcp.method": "callTool"}
		if s != nil {
			if s.MCPServer != "" {
				attrs["mcp.server"] = s.MCPServer
				fields["mcp_server"] = s.MCPServer
			}
			if s.Function != "" {
				attrs["mcp.tool"] = s.Function
				fields["mcp_tool"] = s.Function
			}
			fields["mcp_method"] = "callTool"
		}
	case HookShell, HookTool:
		// No structural family fields to carry; shell_command stays null (INV-2).
	}

	if name == "" {
		name = firstNonEmpty(ev.Tool.Name, string(ev.EventType))
	}
	return name, attrs, fields
}

// sessionTraceID derives a stable 32-hex trace_id for a session from its id, so
// every hook span in the session shares one trace without threading state across
// the separate hook processes / the spool. Any 32-hex value is a wire-valid
// trace_id (AssertHookWireShape); stability per session is the only requirement.
func sessionTraceID(sessionID string) string {
	sum := sha256.Sum256([]byte("openbox-dev-trace\x1f" + sessionID))
	return hex.EncodeToString(sum[:16]) // 16 bytes → 32 lowercase hex
}

// activityPairKey is the string that is IDENTICAL for a tool call's started
// (ToolCall) and completed (ToolResult) events and (best-effort) distinct across
// different tool calls. It excludes the stage and the timestamp — the two fields
// that differ between the paired events — and folds in the session, tool name,
// and the structural file/function locator. All fields survive the SL-4 spool
// round-trip, so the derived ids are stable even after a rehydrated flush.
//
// Limitation: two IDENTICAL sequential tool calls (same tool + same locator in
// one session) share a pair key, so their spans carry the same activity_id/
// span_id. This is acceptable — Claude Code sequences Pre→Post per call and Core
// pairs the open started span — and is documented rather than solved here (a
// per-invocation tool_use_id, if surfaced by a future HookEvent field, would make
// it exact). No content feeds the key (INV-2): the file_path/function are
// structural locators.
func activityPairKey(ev DevEvent) string {
	const sep = 0x1f
	var b strings.Builder
	b.WriteString(ev.SessionID)
	b.WriteByte(sep)
	b.WriteString(ev.Tool.Name)
	if ev.Span != nil {
		b.WriteByte(sep)
		b.WriteString(ev.Span.FilePath)
		b.WriteByte(sep)
		b.WriteString(ev.Span.Function)
	}
	return b.String()
}

// hookActivityID is the wire pairing key (free-form; not hex-constrained) shared
// by a tool call's started and completed events.
func hookActivityID(ev DevEvent) string {
	sum := sha256.Sum256([]byte("act\x1f" + activityPairKey(ev)))
	return "cc-act-" + hex.EncodeToString(sum[:16])
}

// hookSpanID is the 16-hex span_id shared by a tool call's started and completed
// spans (base SDK shared-span pairing — the same span object drives both stages).
func hookSpanID(ev DevEvent) string {
	sum := sha256.Sum256([]byte("span\x1f" + activityPairKey(ev)))
	return hex.EncodeToString(sum[:8]) // 8 bytes → 16 lowercase hex
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
		RequestBody:  strPtr(capBody(s.RequestBody)),
		ResponseBody: strPtr(capBody(s.ResponseBody)),
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

// maxBodySize caps a gated content body before egress, mirroring the base SDK's
// PrivacyConfig.max_body_size default (openbox-sdk-python openbox_core/config.py
// = 65536 chars). shift-left signs the exact bytes buildPayload returns, so
// capping here caps the signed bytes — the base SDK applies the same cap before
// signing (serialization.truncate_string).
const maxBodySize = 65536

// capBody truncates a content body to maxBodySize (G_SEC SEC-1), the Go mirror of
// the base SDK's truncate_string: hard cut, no marker, counted in RUNES to match
// Python's per-character semantics. Bounds egress payload size and enforces the
// product's content-size privacy cap on the opt-in content-capture path. Only
// content bodies (request_body/response_body) are capped — structural
// identifiers (paths, tool/mcp names) are already bounded at the adapter (capStr)
// and shell_command is never carried on the egress path (INV-2).
func capBody(s string) string {
	if len(s) <= maxBodySize { // fast path: byte len ≤ cap ⇒ rune count ≤ cap
		return s
	}
	r := []rune(s)
	if len(r) <= maxBodySize {
		return s
	}
	return string(r[:maxBodySize])
}

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
