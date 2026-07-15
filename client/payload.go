package client

import (
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
	ActivityType string `json:"activity_type,omitempty"`
	WorkflowID   string `json:"workflow_id"`
	RunID        string `json:"run_id"`
	// WorkflowType is the base wire contract's REQUIRED workflow discriminator for
	// every lifecycle AND signal event (openbox-sdk-python validation/event_rules.go
	// _REQUIRED_WORKFLOW_FIELDS = workflow_id/run_id/workflow_type; core reads it
	// into a dedicated column — storage_event.go buildGovernanceEventSetter). Kept
	// CONSTANT per session (workflowType) so WorkflowStarted, its SignalReceived
	// events, and WorkflowCompleted all resolve to one workflow. Absent (omitempty)
	// on the ActivityStarted hook path, which builds its own map envelope.
	WorkflowType string `json:"workflow_type,omitempty"`
	// SignalName is REQUIRED on a SignalReceived event (event_rules.py raises
	// ENVELOPE_MISSING_FIELDS / "missing signal_name" otherwise; core stores it in
	// the SignalName column). Empty (omitted) on Workflow*/hook events.
	SignalName string          `json:"signal_name,omitempty"`
	Timestamp  string          `json:"timestamp"`
	SpanCount  int             `json:"span_count,omitempty"`
	Spans      []spanData      `json:"spans,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

// spanData mirrors the subset of openbox-core's SpanData
// (governance.go:266). Since E7-S4/S5 no production path populates it — tool spans
// emit the flat hook map (BuildHookSpan) and lifecycle events are span-less — it
// now serves as a DECODE-side view: its field tags overlap the flat hook span, so
// tests can read a payload back typed. Note the wire tags that differ from the
// field names: FuncName→"function", SpanID→"span_id". Times are int64
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
// The wire splits by event class (the ADR-0004 unification, fully landed by E7-S5):
//   - ToolCall/ToolResult (E7-S4) serialize onto the base SDK's flat hook shape
//     (ActivityStarted + hook_trigger + a flat SpanData whose `stage` field is
//     the only started-vs-completed distinguisher on the wire — a ToolResult is
//     NOT ActivityCompleted; that value is reserved for hook-less lifecycle
//     events, verified against openbox-sdk-python contracts/events.py
//     `wire_event_type`/`hook` + conformance/fake_core.assert_hook_wire_shape).
//     The started+completed pair share an activity_id and span_id so Core (and
//     the shared dashboard) pair them onto one timeline row.
//   - Everything else (E7-S5) is lifecycle: SessionStarted/Ended become
//     Workflow* (session=workflow), and PromptSubmitted/CommitCreated/Deploy
//     become SignalReceived — all stock accept-listed base wire types, so the
//     SL-13 EXT-core dev-type accept-list is no longer needed (INV-8).
func buildPayload(ev DevEvent) ([]byte, error) {
	if ev.EventType == EventToolCall || ev.EventType == EventToolResult {
		return buildHookPayload(ev)
	}
	return buildLifecyclePayload(ev)
}

// workflowType is the base wire contract's required `workflow_type` for a
// developer session. It is a CONSTANT (not the provider name — that rides in
// metadata) so a session's WorkflowStarted, every SignalReceived it carries, and
// its WorkflowCompleted share one (workflow_id, run_id, workflow_type) identity
// and Core resolves them to the same workflow/session row (storage_session.go
// handleSessionCreate → handleSessionLookup → handleSessionTerminal).
const workflowType = "developer-session"

// buildLifecyclePayload serializes a non-tool DevEvent onto the base SDK's
// lifecycle wire shape (ADR-0004: session=workflow; commit/deploy=signal):
//   - SessionStarted → WorkflowStarted (creates the Core session row)
//   - SessionEnded   → WorkflowCompleted (closes it)
//   - PromptSubmitted/CommitCreated/Deploy → SignalReceived(signal_name)
//
// Every lifecycle event carries workflow_id/run_id/workflow_type (required by the
// base contract for BOTH workflow and signal events); signals additionally carry
// signal_name. This path is deliberately SPAN-LESS: the base contract rejects a
// span-bearing non-hook lifecycle event (event_rules HOOK_TRIGGER_FALSE /
// ACTIVITY_COMPLETED_WITH_SPANS), and no lifecycle DevEvent sets ev.Span (spans
// only ride the ToolCall/ToolResult hook path). Lineage keys for commit/deploy
// (commit_sha, deploy_id, deploy_did, repo, …) have no first-class Core column,
// so they ride the pass-through metadata blob via buildMetadata (FR-5/6/7).
func buildLifecyclePayload(ev DevEvent) ([]byte, error) {
	workflowID := ev.WorkspaceID
	if workflowID == "" {
		workflowID = ev.DeveloperDID // stable per-developer identity fallback
	}

	wireType, signalName := lifecycleWireType(ev.EventType)

	p := governanceEventPayload{
		Source:       source,
		EventType:    wireType,
		ActivityType: activityLabel(ev), // additive dashboard label (pass-through column)
		WorkflowID:   workflowID,
		RunID:        ev.SessionID,
		WorkflowType: workflowType,
		SignalName:   signalName, // "" (omitted) unless this is a SignalReceived
		Timestamp:    ev.Timestamp,
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

// lifecycleWireType maps a developer-runtime lifecycle EventType onto its base
// wire event_type and, for a signal, its signal_name. The DevEvent EventType is
// preserved as the dashboard activity_type label (activityLabel); only the wire
// type is rewritten. An unrecognized non-tool type (none exists today) falls back
// to its own string with no signal_name — defensive, never reached in practice.
func lifecycleWireType(et EventType) (wireType, signalName string) {
	switch et {
	case EventSessionStarted:
		return "WorkflowStarted", ""
	case EventSessionEnded:
		return "WorkflowCompleted", ""
	case EventPromptSubmitted:
		return "SignalReceived", "prompt_submitted"
	case EventCommitCreated:
		return "SignalReceived", "commit_created"
	case EventDeploy:
		return "SignalReceived", "deploy"
	default:
		return string(et), ""
	}
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

	// Compact JSON, signed-once (see buildLifecyclePayload) — the bytes returned are
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

// fileSpanName maps a file semantic_type to the exact span Name core's
// classifier fallback matches (session.go:257-268) to store that file_* type.
var fileSpanName = map[string]string{
	"file_read":   "file.read",
	"file_write":  "file.write",
	"file_open":   "file.open",
	"file_delete": "file.delete",
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
	// Preserve the real tool name: on the hook path the span Name is repurposed to
	// drive core's server-side classification (hookSpanShape), so tool identity
	// would otherwise be lost.
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
