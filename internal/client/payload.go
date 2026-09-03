package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const source = "developer-runtime"

type governanceEventPayload struct {
	Source    string `json:"source"`
	EventType string `json:"event_type"`
	// ActivityType is core's pass-through activity_type column, which the
	// openbox-fe dashboard's "Activity" column reads first. Always set (see
	// activityLabel) so the UI never falls back to "Unknown".
	ActivityType string `json:"activity_type,omitempty"`
	// ActivityID pairs a tool call's ActivityStarted and ActivityCompleted onto
	// one timeline row, and is the approval key: an approval is filed against
	// (workflow_id, run_id, activity_id), the hold polls that triple, and core
	// scopes its bypass grants by it.
	ActivityID string `json:"activity_id,omitempty"`
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id"`
	// WorkflowType is the base wire contract's required workflow discriminator.
	WorkflowType string `json:"workflow_type,omitempty"`
	// SignalName is required on a SignalReceived event, empty on
	// Workflow*/Activity* events.
	SignalName string `json:"signal_name,omitempty"`
	// SignalArgs carries a SignalReceived event's arguments (the openbox-fe
	// Verify-tab "Input" detail reads log.signal_args).
	SignalArgs json.RawMessage `json:"signal_args,omitempty"`
	// ActivityInput rides ActivityStarted; core stores it as the row's `input`
	// and runs Guardrails stage "0" over it (services/guardrail.go:180).
	ActivityInput json.RawMessage `json:"activity_input,omitempty"`
	// ActivityOutput rides ActivityCompleted; core stores it as the row's
	// `output` and runs Guardrails stage "1" over it (services/guardrail.go:192).
	ActivityOutput json.RawMessage `json:"activity_output,omitempty"`
	// DurationMs is how long the tool call took, in milliseconds.
	DurationMs *float64        `json:"duration_ms,omitempty"`
	Timestamp  string          `json:"timestamp"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	// Status is the tool call's outcome, and the single field core's per-tool
	// success metric reads: appended last deliberately.
	Status string `json:"status,omitempty"`
	// No spans: core parses spans[] and discards it. docs/mapping.md §2.
}

const (
	wireWorkflowStarted   = "WorkflowStarted"
	wireWorkflowCompleted = "WorkflowCompleted"
	wireSignalReceived    = "SignalReceived"
	wireActivityStarted   = "ActivityStarted"
	wireActivityCompleted = "ActivityCompleted"
)

// buildPayload the bytes returned here are both hashed for the signature AND
// sent as the body, so they are produced exactly once; client.go never re-
// marshals.
func buildPayload(ev DevEvent) ([]byte, error) {
	wireType, signalName, err := wireTypeFor(ev.EventType)
	if err != nil {
		return nil, err
	}

	p := governanceEventPayload{
		Source:       source,
		EventType:    wireType,
		ActivityType: activityLabel(ev), // additive dashboard label (pass-through column)
		WorkflowID:   workflowIDFor(ev),
		RunID:        ev.SessionID,
		WorkflowType: workflowType,
		SignalName:   signalName, // "" (omitted) unless this is a SignalReceived
		Timestamp:    ev.Timestamp,
	}
	if signalName != "" {
		p.SignalArgs = buildSignalArgs(ev) // nil (omitted) when there is nothing to show
	}

	switch ev.EventType {
	case EventToolCall:
		p.ActivityID = activityIDFor(ev)
		p.ActivityInput = structuralActivityInput(ev)
	case EventToolResult:
		p.ActivityID = activityIDFor(ev)
		p.ActivityOutput = structuralActivityOutput(ev)
		p.DurationMs = durationMs(ev)
		p.Status = statusFor(ev)
	case EventTurnStarted:
		p.ActivityID = turnActivityIDFor(ev)
		p.ActivityInput = turnActivityInput(ev)
	case EventTurnCompleted:
		p.ActivityID = turnActivityIDFor(ev)
		p.ActivityOutput = turnActivityOutput(ev)
		p.DurationMs = durationMs(ev)
		// Deliberately NOT set: hook_trigger, which would route a model turn onto
		// core's approval-bypass path.
	}
	// A turn naming no producer cannot be placed: activity_id is empty and
	// omitempty drops it, so content must not ride the row. Both deleted span
	// builders opened with this check, and it was lost when content moved to the
	// activity fields. Here, not in the two builders: one rule about the ROW.
	if p.ActivityID == "" {
		p.ActivityInput, p.ActivityOutput = nil, nil
	}

	meta, err := buildMetadata(ev)
	if err != nil {
		return nil, err
	}
	p.Metadata = meta

	return json.Marshal(p)
}

const workflowType = "developer-session"

func wireTypeFor(et EventType) (wireType, signalName string, err error) {
	switch et {
	case EventSessionStarted:
		return wireWorkflowStarted, "", nil
	case EventSessionEnded:
		return wireWorkflowCompleted, "", nil
	case EventPromptSubmitted:
		return wireSignalReceived, "prompt_submitted", nil
	case EventCommitCreated:
		return wireSignalReceived, "commit_created", nil
	case EventDeploy:
		return wireSignalReceived, "deploy", nil
	case EventSubagentStarted:
		return wireSignalReceived, "subagent_started", nil
	case EventPermissionDenied:
		return wireSignalReceived, "permission_denied", nil
	case EventAPIError:
		return wireSignalReceived, "api_error", nil
	case EventToolCall, EventTurnStarted:
		return wireActivityStarted, "", nil
	case EventToolResult, EventTurnCompleted:
		return wireActivityCompleted, "", nil
	}
	return "", "", fmt.Errorf("client: no base wire type for event_type %q", et)
}

// activityPairKey identifies the operation a tool call performs: the session,
// the tool, its structural locator, and the operation discriminator the
// adapter derived (see Span.OperationID).
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
		b.WriteByte(sep)
		b.WriteString(ev.Span.OperationID)
	}
	return b.String()
}

func workflowIDFor(ev DevEvent) string {
	if ev.WorkspaceID != "" {
		return ev.WorkspaceID
	}
	return ev.DeveloperDID
}

func activityIDFor(ev DevEvent) string {
	sum := sha256.Sum256([]byte("act\x1f" + activityPairKey(ev)))
	return "cc-act-" + hex.EncodeToString(sum[:16])
}

// turnActivityIDFor is the wire activity_id shared by a turn's ActivityStarted
// and ActivityCompleted: "<session_id>:turn:<index>", or
// "<session_id>:agent:<agent_id>:turn:<index>" for a subagent's turn.
// Three properties, and the id is built rather than hashed for the first:
//   - There is nothing to hash.
//   - It must be derivable from fields that survive the spool (SessionID,
//     TurnIndex, AgentID are all persisted), because a flush can happen long
//     after the hook process that built the event exited.
//   - It cannot collide with a tool-call id by construction: those are "cc-
//     act-" + 32 hex chars, and this shape contains ':' and a decimal index.
func turnActivityIDFor(ev DevEvent) string {
	if ev.ProxyRequestID != "" {
		return ev.SessionID + ":proxy:" + ev.ProxyRequestID
	}
	if ev.GatewayRequestID != "" {
		return ev.SessionID + ":gateway:" + ev.GatewayRequestID
	}
	if ev.OtelRequestID != "" {
		return ev.SessionID + ":otel:" + ev.OtelRequestID
	}
	if ev.SessionRollup {
		return ev.SessionID + ":usage:rollup"
	}
	if ev.TurnIndex == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(ev.SessionID)
	if ev.AgentID != "" {
		b.WriteString(":agent:")
		b.WriteString(ev.AgentID)
	}
	b.WriteString(":turn:")
	b.WriteString(strconv.Itoa(*ev.TurnIndex))
	return b.String()
}

// turnActivityOutput cost is deliberately absent.
func turnActivityOutput(ev DevEvent) json.RawMessage {
	m := map[string]any{}
	if ev.Model != "" {
		m["model"] = ev.Model
	}
	if t := ev.Tokens; t != nil {
		usage := map[string]any{}
		if t.Input != nil {
			usage["input_tokens"] = *t.Input
		}
		if t.Output != nil {
			usage["output_tokens"] = *t.Output
		}
		if t.CacheCreationInput != nil {
			usage["cache_creation_input_tokens"] = *t.CacheCreationInput
		}
		if t.CacheRead != nil {
			usage["cache_read_input_tokens"] = *t.CacheRead
		}
		if len(usage) > 0 {
			m["usage"] = usage
		}
	}
	if ev.Content != nil && ev.Content.Thinking != "" {
		m["thinking"] = capBody(ev.Content.Thinking)
	}
	// What the model said; the two sources are mutually exclusive.
	switch {
	case ev.Span != nil && ev.Span.ResponseBody != "":
		// Verbatim, SSE frames and all: reassembly belongs with the consumer.
		m[modelCallContentKey] = capModelCallBody(ev.Span.ResponseBody)
	case ev.Content != nil && ev.Content.Output != "":
		m[modelCallContentKey] = capModelCallBody(ev.Content.Output)
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

var toolStatuses = map[string]bool{
	StatusCompleted: true,
	StatusFailed:    true,
}

func statusFor(ev DevEvent) string {
	if ev.EventType != EventToolResult {
		return ""
	}
	if !toolStatuses[ev.Status] {
		return ""
	}
	return ev.Status
}

// activityLabel identifier-class only; a tool name, a fixed label, or an event
// type; never content (INV-2).
//   - A tool event (ToolCall/ToolResult) → the specific tool name ("Edit"/
//     "Bash"/"mcp__…"), the most useful Activity label;
//   - A turn event → "llm_completion", the same label on both halves, so the
//     core-side usage extractor has one key to select on and the two runtimes
//     share one vocabulary;
//   - Everything else (lifecycle, Deploy) → the event_type string.
func activityLabel(ev DevEvent) string {
	// Only from the closed vocabulary: this column is pass-through, so a typo would
	// reach the dashboard unfiltered.
	if slices.Contains(AllActivityTypes, ev.ActivityType) {
		return ev.ActivityType
	}
	switch ev.EventType {
	case EventToolCall, EventToolResult:
		if ev.Tool.Name != "" {
			return ev.Tool.Name
		}
	case EventTurnStarted, EventTurnCompleted:
		return ActivityTypeLLMCompletion
	}
	return string(ev.EventType)
}

// contentMetadataKeys are metadata keys that carry free text rather than a
// structural identifier.
//
// A key here is dropped when content capture is off, exactly like Content. It is
// a backstop against an adapter putting content in metadata, not a content
// classifier, and it must name every content key or an adapter writing one there
// routes around the gate.
var contentMetadataKeys = map[string]bool{
	"message":       true, // a commit message body
	"prompt":        true,
	"output":        true,
	"content":       true,
	"file_text":     true,
	"diff":          true,
	"patch":         true,
	"body":          true,
	"stdout":        true,
	"stderr":        true,
	"command":       true,
	"input_text":    true,
	"denial_reason": true,
	"error_details": true,
	"arguments":     true,
	"thinking":      true,
}

// observesAResponse is which half of an activity may assert an HTTP status.
//
// A Started row represents a request that has not been answered, so a status code
// on one is a claim about a response that did not exist when the row was made --
// and 116 of 116 live Started rows asserted `200`. The cause was structural:
// gatewayemit builds both halves from one shared `span(stage)` closure
// (`internal/cli/gatewayemit/event.go:84-93`) that copies HTTPStatus onto each.
// So the condition lives here, in the one funnel every adapter passes through,
// rather than in the adapter that exposed it.
//
// It gates the metadata KEY and not only the span field, because buildMetadata
// copies caller metadata in first: an adapter writing `http_status` by hand would
// otherwise reinstate exactly the assertion this removes.
//
// Removing it from the Started half removes a falsehood, not a control. The
// Completed half still carries it, ungated by content capture, which is what lets
// a 5xx and a transport failure store differently from a success whose reply was
// not captured.
func observesAResponse(et EventType) bool {
	switch et {
	case EventToolResult, EventTurnCompleted:
		return true
	}
	return false
}

func signalDetailKeyFor(t EventType) string {
	switch t {
	case EventPermissionDenied:
		return "denial_reason"
	case EventAPIError:
		return "error_details"
	}
	return ""
}

func buildMetadata(ev DevEvent) (json.RawMessage, error) {
	m := make(map[string]any, len(ev.Metadata)+4)
	for k, v := range ev.Metadata {
		if ev.contentStripped && contentMetadataKeys[k] {
			continue // INV-2: gated content never rides the metadata blob either
		}
		if k == "http_status" && !observesAResponse(ev.EventType) {
			continue // see observesAResponse; the key is barred by the ROW's meaning
		}
		m[k] = v
	}
	if ev.Content != nil && ev.Content.SignalDetail != "" {
		if k := signalDetailKeyFor(ev.EventType); k != "" {
			m[k] = capBody(ev.Content.SignalDetail)
		}
	}
	m["event_id"] = ev.EventID
	if ev.Tool.Name != "" {
		m["tool_name"] = ev.Tool.Name
	}
	if ev.Tokens != nil {
		m["tokens"] = ev.Tokens
	}
	if ev.Cost != nil {
		m["cost"] = ev.Cost
	}
	if ev.Model != "" {
		if _, exists := m["model"]; !exists {
			m["model"] = ev.Model
		}
	}
	if ev.AgentID != "" {
		if _, exists := m["agent_id"]; !exists {
			m["agent_id"] = ev.AgentID
		}
	}
	// Rehomed from span attributes, which never persisted. NOT in
	// contentMetadataKeys: derived evidence, not content.
	if s := ev.Span; s != nil {
		if s.CredentialFingerprint != "" {
			if _, exists := m["credential_fingerprint"]; !exists {
				m["credential_fingerprint"] = s.CredentialFingerprint
			}
		}
		if s.HTTPStatus != 0 && observesAResponse(ev.EventType) {
			if _, exists := m["http_status"]; !exists {
				m["http_status"] = s.HTTPStatus
			}
		}
		// Here and not only in activity_input, which the content gate empties: the
		// method and URL are account-binding evidence, and docs/data-and-privacy.md
		// says they ship under content_capture:false. They used to ride a span
		// field stripContent never cleared; once that went, capture-off stored a
		// relayed call with no method or URL anywhere. Duplicated with
		// activity_input rather than moved, where being unlisted makes the
		// alignment judge's cap drop the pair before `content`.
		if s.HTTPMethod != "" {
			if _, exists := m["http_method"]; !exists {
				m["http_method"] = s.HTTPMethod
			}
		}
		if s.HTTPURL != "" {
			if _, exists := m["http_url"]; !exists {
				m["http_url"] = s.HTTPURL
			}
		}
	}
	return json.Marshal(m)
}

// structuralActivityInput builds the INV-2-safe `activity_input` for an
// ActivityStarted: the identifiers the Verify tab's "Input" detail renders, and
// what the control plane runs its first Guardrails stage over.
func contentKeyFor(kind ToolKind) string {
	switch kind {
	case ToolShell:
		return "command"
	case ToolMCP:
		return "arguments"
	case ToolFile:
		return "content"
	}
	return "arguments"
}

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
	if ev.Content != nil && ev.Content.ToolInput != "" {
		m[contentKeyFor(ev.Tool.Kind)] = capBody(ev.Content.ToolInput)
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

// structuralActivityOutput returns nil (field omitted) when nothing is known;
// which is the honest state for a shell call, whose counts the providers do
// not expose.
func structuralActivityOutput(ev DevEvent) json.RawMessage {
	m := map[string]any{}
	if s := ev.Span; s != nil {
		if s.BytesRead != nil {
			m["bytes_read"] = *s.BytesRead
		}
		if s.BytesWritten != nil {
			m["bytes_written"] = *s.BytesWritten
		}
		if s.LinesCount != nil {
			m["lines_count"] = *s.LinesCount
		}
	}
	if v, ok := ev.Metadata["exit_code"]; ok {
		m["exit_code"] = v
	}
	if ev.Content != nil && ev.Content.ToolOutput != "" {
		m["output"] = capBody(ev.Content.ToolOutput)
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

func durationMs(ev DevEvent) *float64 {
	start := rfc3339Nanos(firstNonEmpty(ev.StartedAt, ev.Timestamp))
	end := rfc3339Nanos(firstNonEmpty(ev.EndedAt, ev.Timestamp))
	if start == 0 || end <= start {
		return nil
	}
	ms := float64(end-start) / float64(time.Millisecond)
	return &ms
}

func buildSignalArgs(ev DevEvent) json.RawMessage {
	m := map[string]any{}
	switch ev.EventType {
	case EventPromptSubmitted:
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

func copyMetaKeys(dst, src map[string]any, keys ...string) {
	for _, k := range keys {
		if v, ok := src[k]; ok {
			dst[k] = v
		}
	}
}

// stripContent the caller's event is never mutated.
func stripContent(ev DevEvent) DevEvent {
	ev.contentStripped = true
	ev.Content = nil
	if ev.Span != nil {
		s := *ev.Span // copy so the caller's Span is untouched
		s.RequestBody = ""
		s.ResponseBody = ""
		s.RequestHeaders = nil
		s.ResponseHeaders = nil
		ev.Span = &s
	}
	return ev
}

const maxBodySize = 65536

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

// modelCallContentKey is load-bearing: Goal Alignment's cap emits a fixed
// priority list, then unlisted keys, then breaks. `content` is in the list.
const modelCallContentKey = "content"

// maxModelCallBodyBytes bounds a relayed body in BYTES, and the unit is the decision:
// capBody tests bytes then cuts runes, so a 64Ki-rune CJK body reaches 192 KB, while
// this bounds core's synchronous evaluation cost. One constant, so the pending live
// measurement changes one line. Its own literal, NOT `= maxBodySize`: that one is a
// RUNE bound, so raising it for longer thinking would silently change this byte cap
// too. Equal today, and that is a coincidence worth keeping visible.
//
// MaxModelCallBodyBytes is exported so internal/gateway can assert its own
// selection budget stays strictly below this, in the package that owns the number.
// The alternative was a duplicated literal in a gateway test, which reds only if
// someone edits the copy too -- a guard that passes whether or not the invariant
// holds. Import direction allows this: gateway already imports client, never the
// reverse.
const MaxModelCallBodyBytes = 65536

const maxModelCallBodyBytes = MaxModelCallBodyBytes

func capModelCallBody(s string) string {
	if len(s) <= maxModelCallBodyBytes {
		return s
	}
	return trimTrailingPartialRune(s[:maxModelCallBodyBytes-len(truncationMark)]) + truncationMark
}

// truncationMark makes a shortened body distinguishable from a complete one: a
// clipped reply otherwise reaches OPA and an auditor looking finished.
const truncationMark = "…[openbox: truncated]"

func trimTrailingPartialRune(s string) string {
	for i := len(s); i > 0 && i > len(s)-utf8.UTFMax; i-- {
		if r, size := utf8.DecodeLastRuneInString(s[:i]); r != utf8.RuneError || size > 1 {
			return s[:i]
		}
	}
	return s
}

// turnActivityInput is the observed request on the opening half, and IS an
// in-path lane's alignment input. A hook turn yields nil, which is required.
func turnActivityInput(ev DevEvent) json.RawMessage {
	s := ev.Span
	if s == nil || s.RequestBody == "" {
		// A non-empty activity_input is what makes core judge an operation, and the
		// http_* pair alone would file a probe as work that never happened.
		return nil
	}
	m := map[string]any{modelCallContentKey: capModelCallRequest(s.RequestBody)}
	// Unlisted, so the judge's cap drops these before `content`.
	if s.HTTPMethod != "" {
		m["http_method"] = s.HTTPMethod
	}
	if s.HTTPURL != "" {
		m["http_url"] = s.HTTPURL
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

// capModelCallRequest keeps the TAIL: a /v1/messages body is a conversation whose
// NEWEST turn is at the end, so a head cut keeps only the boilerplate.
func capModelCallRequest(s string) string {
	if len(s) <= maxModelCallBodyBytes {
		return s
	}
	tail := s[len(s)-(maxModelCallBodyBytes-len(truncationMark)):]
	for i := 0; i < len(tail) && i < utf8.UTFMax; i++ {
		if utf8.RuneStart(tail[i]) {
			return truncationMark + tail[i:]
		}
	}
	return truncationMark + tail
}
