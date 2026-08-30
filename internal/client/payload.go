package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
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
	// Spans carries exactly ONE span, on a TurnCompleted under content capture,
	// and nothing else ever. It exists because core's goal-alignment extractor
	// reads assistant text from payload.Spans and from no other field, so a span-
	// less session can never feed it; see client/turnspan.go.
	Spans     []wireSpan `json:"spans,omitempty"`
	SpanCount int        `json:"span_count,omitempty"`
}

const (
	wireWorkflowStarted   = "WorkflowStarted"
	wireWorkflowCompleted = "WorkflowCompleted"
	wireSignalReceived    = "SignalReceived"
	wireActivityStarted   = "ActivityStarted"
	wireActivityCompleted = "ActivityCompleted"
)

// buildPayload maps a normalized DevEvent onto core's GovernanceEventPayload
// and marshals it to the exact bytes that will be signed and transmitted.
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
	case EventTurnCompleted:
		p.ActivityID = turnActivityIDFor(ev)
		p.ActivityOutput = turnActivityOutput(ev)
		p.DurationMs = durationMs(ev)
		// Note what is deliberately NOT set alongside: hook_trigger.
		if span := observedSpan(ev); span != nil {
			p.Spans = []wireSpan{*span}
			p.SpanCount = 1
		} else if span := turnAssistantSpan(ev); span != nil {
			p.Spans = []wireSpan{*span}
			p.SpanCount = 1
		}
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

// workflowIDFor is the wire workflow_id: the workspace identity, falling back
// to the stable per-developer one.
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

const activityTypeLLMCompletion = "llm_completion"

// turnActivityOutput builds the `activity_output` for a turn's
// ActivityCompleted: the model that ran and the four token counts it spent.
// Cost is deliberately absent.
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

// activityLabel resolves the human-readable action label emitted as core's
// pass-through `activity_type` column (openbox-fe's "Activity" column reads it
// first and shows "Unknown" when absent).
func activityLabel(ev DevEvent) string {
	switch ev.EventType {
	case EventToolCall, EventToolResult:
		if ev.Tool.Name != "" {
			return ev.Tool.Name
		}
	case EventTurnStarted, EventTurnCompleted:
		return activityTypeLLMCompletion
	}
	return string(ev.EventType)
}

// contentMetadataKeys buildMetadata merges the caller's per-type metadata with
// the finops keys (tokens/cost), the true tool name, and the idempotency key
// (mapping.md §1).
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
	return json.Marshal(m)
}

// contentKeyFor structuralActivityInput builds the INV-2-safe `activity_input`
// for an ActivityStarted; the identifiers the openbox-fe Verify-tab "Input"
// detail renders (log.input), and what core runs Guardrails stage "0" over
// (services/guardrail.go:180).
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

// structuralActivityOutput builds the INV-2-safe `activity_output` for an
// ActivityCompleted; what core stores as the row's `output` and runs
// Guardrails stage "1" over (services/guardrail.go:192).
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

// durationMs is how long a tool call took, in float milliseconds, for the
// ActivityCompleted payload.
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

// stripContent returns a copy of ev with every gated content field removed
// (INV-2). The caller's event is never mutated.
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
