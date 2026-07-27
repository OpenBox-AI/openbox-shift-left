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
	// ActivityType is core's pass-through activity_type column, which the
	// openbox-fe dashboard's "Activity" column reads first. Always set (see
	// activityLabel) so the UI never falls back to "Unknown".
	ActivityType string `json:"activity_type,omitempty"`
	WorkflowID   string `json:"workflow_id"`
	RunID        string `json:"run_id"`
	// WorkflowType is the base wire contract's required workflow discriminator
	// for every lifecycle and signal event. Kept constant per session
	// (workflowType) so WorkflowStarted, its SignalReceived events, and
	// WorkflowCompleted all resolve to one workflow. Absent on the
	// ActivityStarted hook path, which builds its own map envelope.
	WorkflowType string `json:"workflow_type,omitempty"`
	// SignalName is required on a SignalReceived event, empty on
	// Workflow*/hook events.
	SignalName string `json:"signal_name,omitempty"`
	// SignalArgs carries a SignalReceived event's arguments (the openbox-fe
	// Verify-tab "Input" detail reads log.signal_args). Commit/deploy signals
	// carry structural lineage identifiers (commit_sha/repo/deploy_id/…); a
	// prompt_submitted signal carries the prompt only under content-capture
	// (content — INV-2 — gated like a request_body, capped, absent by
	// default), never a commit-message body or session context. See
	// buildSignalArgs.
	SignalArgs json.RawMessage `json:"signal_args,omitempty"`
	Timestamp  string          `json:"timestamp"`
	SpanCount  int             `json:"span_count,omitempty"`
	Spans      []spanData      `json:"spans,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

// spanData mirrors the subset of openbox-core's SpanData (governance.go:266).
// No production path populates it — tool spans emit the flat hook map
// (BuildHookSpan) and lifecycle events are span-less — so it now serves as a
// decode-side view: its field tags overlap the flat hook span, so tests can
// read a payload back typed. Note the wire tags that differ from the field
// names: FuncName→"function", SpanID→"span_id". Times are int64 epoch
// nanoseconds (core's OTel convention).
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
// and marshals it to the exact bytes that will be signed and transmitted.
// Content-stripping (INV-2) has already run in Emit when content-capture is
// disabled, so any content still present here is authorized.
//
// The wire splits by event class (the ADR-0004 unification):
//   - ToolCall/ToolResult serialize onto the base SDK's flat hook shape
//     (ActivityStarted + hook_trigger + a flat SpanData whose `stage` field is
//     the only started-vs-completed distinguisher on the wire — a ToolResult
//     is not ActivityCompleted; that value is reserved for hook-less
//     lifecycle events). The started+completed pair share an activity_id and
//     span_id so Core (and the shared dashboard) pair them onto one timeline
//     row.
//   - Everything else is lifecycle: SessionStarted/Ended become Workflow*
//     (session=workflow), and PromptSubmitted/CommitCreated/Deploy become
//     SignalReceived — all stock accept-listed base wire types (INV-8).
func buildPayload(ev DevEvent) ([]byte, error) {
	if ev.EventType == EventToolCall || ev.EventType == EventToolResult {
		return buildHookPayload(ev)
	}
	return buildLifecyclePayload(ev)
}

// workflowType is the base wire contract's required `workflow_type` for a
// developer session. It's a constant (not the provider name — that rides in
// metadata) so a session's WorkflowStarted, every SignalReceived it carries,
// and its WorkflowCompleted share one (workflow_id, run_id, workflow_type)
// identity and Core resolves them to the same workflow/session row.
const workflowType = "developer-session"

// buildLifecyclePayload serializes a non-tool DevEvent onto the base SDK's
// lifecycle wire shape (ADR-0004: session=workflow; commit/deploy=signal):
//   - SessionStarted → WorkflowStarted (creates the Core session row)
//   - SessionEnded   → WorkflowCompleted (closes it)
//   - PromptSubmitted/CommitCreated/Deploy → SignalReceived(signal_name)
//
// Every lifecycle event carries workflow_id/run_id/workflow_type (required
// for both workflow and signal events); signals additionally carry
// signal_name. This path is deliberately span-less: the base contract rejects
// a span-bearing non-hook lifecycle event, and no lifecycle DevEvent sets
// ev.Span (spans only ride the ToolCall/ToolResult hook path). Lineage keys
// for commit/deploy (commit_sha, deploy_id, deploy_did, repo, …) have no
// first-class Core column, so they ride the pass-through metadata blob via
// buildMetadata.
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
	if signalName != "" {
		p.SignalArgs = buildSignalArgs(ev) // nil (omitted) when there is nothing to show
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

// lifecycleWireType maps a developer-runtime lifecycle EventType onto its
// base wire event_type and, for a signal, its signal_name. The DevEvent
// EventType is preserved as the dashboard activity_type label
// (activityLabel); only the wire type is rewritten. An unrecognized non-tool
// type falls back to its own string with no signal_name — defensive, never
// reached in practice.
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
// hook wire shape via BuildHookSpan/BuildHookEvent. The result carries the
// ActivityStarted+hook envelope (event_type/hook_trigger/activity_id/
// activity_type/span_count/spans) merged with the session-attach fields Core
// needs (source, workflow_id, run_id, timestamp, metadata) — as the base
// SDK's to_payload_dict merges the ActivityContext onto the flat top-level
// dict.
func buildHookPayload(ev DevEvent) ([]byte, error) {
	workflowID := ev.WorkspaceID
	if workflowID == "" {
		workflowID = ev.DeveloperDID // stable per-developer identity fallback
	}

	// One session (one Core run) owns one stable trace. Hooks fire as
	// separate short-lived processes, so there's no shared in-memory
	// TraceContext to thread; the trace_id is instead derived deterministically
	// from the session id, so every span in a session shares it without
	// persisting any state.
	tc := TraceContextFrom(sessionTraceID(ev.SessionID))
	span := buildHookSpan(tc, ev)

	// activity_type = the specific tool name (the dashboard's Activity label);
	// activity_id = the deterministic pairing key shared by this call's
	// started and completed spans.
	body := BuildHookEvent(hookActivityID(ev), activityLabel(ev), span)
	body["source"] = source
	body["workflow_id"] = workflowID
	body["run_id"] = ev.SessionID
	if ev.Timestamp != "" {
		body["timestamp"] = ev.Timestamp
	}
	// activity_input rides the started (ToolCall) event, which creates the
	// Core governance_event row. A ToolResult re-enters the existing event
	// via the hook/span path and does not rewrite the event fields, so
	// setting it there would be ignored. Structural only (INV-2): tool/file/
	// mcp identifiers, never command/file content.
	if ev.EventType == EventToolCall {
		if in := structuralActivityInput(ev); in != nil {
			body["activity_input"] = in
		}
	}

	meta, err := buildMetadata(ev)
	if err != nil {
		return nil, err
	}
	// json.RawMessage marshals as the raw object (not a quoted string) when
	// nested in the map, so metadata emits as a nested object exactly as the
	// struct path.
	body["metadata"] = json.RawMessage(meta)

	return json.Marshal(body)
}

// buildHookSpan turns a tool DevEvent into one flat Core SpanData. The
// started (ToolCall) and completed (ToolResult) spans of the same tool call
// share a deterministic span_id (base SDK shared-span pairing) and carry the
// family + attribute source fields Core's classifier reads.
//
// Unlike the base SDK — which shares one span object across both stages, so
// start_time is identical — shift-left builds the two spans in separate hook
// processes from separate events. Claude Code's PostToolUse exposes no start
// time on its own, so a completed span relies on ev.StartedAt having been
// recovered cross-process (adapters/claude-code/duration.go's stash); when
// that recovery misses, start_time falls back to the span's own timestamp and
// duration_ns is 0 — still wire-shape-valid (AssertHookWireShape constrains
// neither the duration sign nor non-zero-ness).
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

// hookTypeFor maps the tool kind (shell|file|mcp) onto its hook type. The
// mapping is 1:1 and provider-agnostic. The generic `tool` hook type covers
// any future kind outside the current taxonomy.
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

// hookSpanShape derives the span name, classifier attributes, and root
// family/body fields for a tool DevEvent:
//   - file ops → name "file.read"/"file.write"/… + root file_path/
//     file_operation/byte counts, so Core's fallback classifier (span.Name +
//     non-nil file_path) stores the file_* semantic type.
//   - mcp → attributes["mcp.method"]="callTool" (the only key Core reads
//     today for mcp_tool_call) plus the structural mcp.server/mcp.tool hints
//     and the mcp_* root family identifiers.
//   - shell → hook_type=shell only; shell_command is content and is never
//     carried on the observe/egress path (read solely for the local enforce
//     decision — INV-2), so it stays present-but-null; shell_exit_code isn't
//     exposed by the CC hook payload.
//
// Gated content bodies (request_body/response_body) ride at the span root and
// only when still present (content-capture on; stripContent nulled them
// otherwise — INV-2), size-capped to maxBodySize before egress (capBody).
// Every family field the caller doesn't supply is filled present-but-null by
// BuildHookSpan, so the payload passes AssertHookWireShape.
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

// sessionTraceID derives a stable 32-hex trace_id for a session from its id,
// so every hook span in the session shares one trace without threading state
// across the separate hook processes / the spool. Any 32-hex value is
// wire-valid (AssertHookWireShape); stability per session is the only
// requirement.
func sessionTraceID(sessionID string) string {
	sum := sha256.Sum256([]byte("openbox-dev-trace\x1f" + sessionID))
	return hex.EncodeToString(sum[:16]) // 16 bytes → 32 lowercase hex
}

// activityPairKey is the string that is identical for a tool call's started
// (ToolCall) and completed (ToolResult) events and (best-effort) distinct
// across different tool calls. It excludes the stage and the timestamp — the
// two fields that differ between the paired events — and folds in the
// session, tool name, and the structural file/function locator. All fields
// survive the adapter's spool round-trip, so the derived ids are stable even
// after a rehydrated flush.
//
// Limitation: two identical sequential tool calls (same tool + same locator
// in one session) share a pair key, so their spans carry the same
// activity_id/span_id. Acceptable — Claude Code sequences Pre→Post per call
// and Core pairs the open started span — and documented rather than solved
// here (a per-invocation tool_use_id, if surfaced by a future HookEvent
// field, would make it exact). No content feeds the key (INV-2): the
// file_path/function are structural locators.
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
// pass-through `activity_type` column (openbox-fe's "Activity" column reads
// it first and shows "Unknown" when absent). It's derived only from fields
// that survive the adapter's spool round-trip (EventType + Tool.Name are
// persisted; a `json:"-"` field would not), so a spooled tool call still
// lands its specific tool name:
//   - a tool event (ToolCall/ToolResult) → the specific tool name ("Edit"/
//     "Bash"/"mcp__…"), the most useful Activity label;
//   - everything else (lifecycle, Deploy) → the event_type string.
//
// Always non-empty. Identifier-class only — a tool name or an event type —
// never content (INV-2).
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
// Never carries content (INV-2) or credentials (INV-1) — those are excluded
// by construction.
//
// event_id goes here deliberately (INV-5): core has no first-class event_id
// field and does not dedupe the developer event types today, so carrying the
// key in metadata is the only way it reaches the wire for a future
// server-side dedupe. Within a single Emit the retried body is
// byte-identical, so the id is stable across attempts.
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

// structuralActivityInput builds the INV-2-safe `activity_input` for a tool
// call — the identifiers the openbox-fe Verify-tab "Input" detail renders
// (log.input). It carries only structural fields: the tool name/kind and the
// file/mcp locators. It never carries content — the shell command, file
// text, request body, or tool arguments — which stay gated on the span
// request_body/response_body (content-capture path). Returns nil (field
// omitted) when nothing structural is known.
func structuralActivityInput(ev DevEvent) json.RawMessage {
	m := map[string]any{}
	if ev.Tool.Name != "" {
		m["tool_name"] = ev.Tool.Name
	}
	if ev.Tool.Kind != "" {
		m["kind"] = string(ev.Tool.Kind)
	}
	if s := ev.Span; s != nil {
		if s.FilePath != "" {
			m["file_path"] = s.FilePath
		}
		if s.FileOp != "" {
			m["file_operation"] = s.FileOp
		}
		if ev.Tool.Kind == ToolMCP {
			if server := firstNonEmpty(s.MCPServer, ev.Tool.MCPServer); server != "" {
				m["mcp_server"] = server
			}
			if s.Function != "" {
				m["mcp_tool"] = s.Function
			}
		}
	}
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// buildSignalArgs builds the `signal_args` for a SignalReceived event — what
// the Verify-tab "Input" detail renders (log.signal_args). The right "input"
// for a signal depends on the signal:
//   - prompt_submitted: the prompt is the argument, and it is content
//     (INV-2), so it's gated exactly like a tool's request_body — carried
//     (capped) only when content-capture is enabled (ev.Content survives
//     stripContent; nil by default), never structural session context.
//   - commit_created / deploy: the arguments are structural lineage
//     identifiers (commit_sha/repo/deploy_id/…), always safe to surface
//     (they also ride metadata).
//
// Returns nil (field omitted) when there is nothing to show — which, for a
// prompt under the default metadata-only posture, is the correct/honest
// state.
func buildSignalArgs(ev DevEvent) json.RawMessage {
	m := map[string]any{}
	switch ev.EventType {
	case EventPromptSubmitted:
		// Content-gated: present only when content-capture kept ev.Content
		// (Emit's stripContent nulls it by default — INV-2). Capped before
		// egress like any body (capBody).
		if ev.Content != nil && ev.Content.Prompt != "" {
			m["prompt"] = capBody(ev.Content.Prompt)
		}
	case EventCommitCreated:
		copyMetaKeys(m, ev.Metadata, "commit_sha", "repo", "branch")
	case EventDeploy:
		copyMetaKeys(m, ev.Metadata, "deploy_id", "commit_sha", "repo", "environment", "deploy_did")
	}
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// copyMetaKeys copies the named keys from src into dst when present. Used to
// lift the structural lineage identifiers into signal_args (they also stay
// in metadata).
func copyMetaKeys(dst, src map[string]any, keys ...string) {
	for _, k := range keys {
		if v, ok := src[k]; ok {
			dst[k] = v
		}
	}
}

// stripContent returns a copy of ev with every gated content field removed
// (INV-2). The caller's event is never mutated. This is the default path
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

// maxBodySize caps a gated content body before egress, mirroring the base
// SDK's PrivacyConfig.max_body_size default (65536 chars). shift-left signs
// the exact bytes buildPayload returns, so capping here caps the signed
// bytes — the base SDK applies the same cap before signing.
const maxBodySize = 65536

// capBody truncates a content body to maxBodySize, the Go mirror of the base
// SDK's truncate_string: hard cut, no marker, counted in runes to match
// Python's per-character semantics. Only content bodies (request_body/
// response_body) are capped — structural identifiers (paths, tool/mcp names)
// are already bounded at the adapter (capStr) and shell_command is never
// carried on the egress path (INV-2).
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
