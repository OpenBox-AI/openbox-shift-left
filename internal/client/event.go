// Package client is the OpenBox developer-runtime data-plane client: the
// shared transport every adapter and the git action use to emit a normalized
// developer event to OpenBox; build the openbox-core GovernanceEventPayload,
// AIP Ed25519-sign the request, POST it to /api/v1/governance/evaluate, and
// parse the verdict.
//   - INV-1: the obx_ API key and the Ed25519 signing seed are never logged or
//     placed on an argv; they live only in the Client and request headers.
//   - INV-2: content (prompt/output/file/tool bodies) is stripped before
//     egress unless content-capture is explicitly enabled for the org.
//   - INV-3: fail-open.
package client

// SchemaVersion is the dev-event contract version this client speaks.
const SchemaVersion = "1.6"

// EventType is a developer-runtime lifecycle event type.
type EventType string

const (
	EventSessionStarted  EventType = "SessionStarted"
	EventPromptSubmitted EventType = "PromptSubmitted"
	EventToolCall        EventType = "ToolCall"
	EventToolResult      EventType = "ToolResult"
	EventSessionEnded    EventType = "SessionEnded"
	// EventCommitCreated is reserved: the wire mapping exists but no adapter
	// produces it.
	EventCommitCreated EventType = "CommitCreated"
	EventDeploy        EventType = "Deploy"

	// EventTurnStarted / EventTurnCompleted are one model turn's boundaries: the
	// unit a coding agent spends tokens in.
	EventTurnStarted   EventType = "TurnStarted"
	EventTurnCompleted EventType = "TurnCompleted"

	// EventSubagentStarted marks a subagent spawning.
	EventSubagentStarted EventType = "SubagentStarted"
	// EventPermissionDenied records that a policy or classifier refused a tool
	// call; that a decision happened, which tool it was about and under the
	// content gate, why (that decision: the provider's free-text `reason` rides
	// Content.SignalDetail → metadata.denial_reason). Never the tool's content.
	EventPermissionDenied EventType = "PermissionDenied"
	// EventAPIError records a turn that ended in a provider-side error rather
	// than an answer (rate limit, billing, auth, overload).
	EventAPIError EventType = "APIError"
)

// AllEventTypes is the complete vocabulary, so callers that need to enumerate
// it (contract cross-checks, test fixtures, coverage bookkeeping) can read it
// from the constants instead of re-typing the list.
var AllEventTypes = []EventType{
	EventSessionStarted,
	EventPromptSubmitted,
	EventToolCall,
	EventToolResult,
	EventSessionEnded,
	EventCommitCreated,
	EventDeploy,
	EventTurnStarted,
	EventTurnCompleted,
	EventSubagentStarted,
	EventPermissionDenied,
	EventAPIError,
}

// So the vocabulary is closed and statusFor drops anything outside it rather
// than forwarding a value core will silently score as a failure.
const (
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// ToolKind is the provider-agnostic tool class ($defs.tool.kind).
type ToolKind string

const (
	ToolShell ToolKind = "shell" // command execution
	ToolFile  ToolKind = "file"  // file read/write/open/delete
	ToolMCP   ToolKind = "mcp"   // MCP tool call
)

// Tool identifies the developer tool/action that produced an event.
type Tool struct {
	Name      string   `json:"name"`
	Kind      ToolKind `json:"kind"`
	MCPServer string   `json:"mcp_server,omitempty"` // required when Kind==ToolMCP
}

// Tokens is per-turn/per-session token usage (metadata only; absent when
// unknown).
type Tokens struct {
	Input  *int `json:"input,omitempty"`
	Output *int `json:"output,omitempty"`
	Total  *int `json:"total,omitempty"`
	// CacheCreationInput is input tokens written to the prompt cache (Anthropic
	// cache_creation_input_tokens; Codex cache_write_input_tokens).
	CacheCreationInput *int `json:"cache_creation_input,omitempty"`
	// CacheRead is input tokens served from the prompt cache (Anthropic
	// cache_read_input_tokens; Codex cached_input_tokens).
	CacheRead *int `json:"cache_read,omitempty"`
}

// Cost is per-turn/per-tool monetary cost (metadata only; absent when
// unknown).
type Cost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency,omitempty"`
}

// Span is one semantic span for a tool call/result.
type Span struct {
	SemanticType string `json:"semantic_type"`
	Stage        string `json:"stage"` // "started" (ToolCall) | "completed" (ToolResult)
	FilePath     string `json:"file_path,omitempty"`
	FileOp       string `json:"file_operation,omitempty"`
	BytesRead    *int   `json:"bytes_read,omitempty"`
	BytesWritten *int   `json:"bytes_written,omitempty"`
	LinesCount   *int   `json:"lines_count,omitempty"`
	Function     string `json:"function,omitempty"`
	Module       string `json:"module,omitempty"`
	MCPServer    string `json:"mcp_server,omitempty"`

	// InvocationID and OperationID separate the two identities a tool call has.
	InvocationID string `json:"invocation_id,omitempty"`
	OperationID  string `json:"operation_id,omitempty"`

	// RequestBody/ResponseBody are gated content (INV-2): stripped before egress
	// unless content-capture is enabled for the org.
	RequestBody  string `json:"request_body,omitempty"`
	ResponseBody string `json:"response_body,omitempty"`

	// RequestHeaders/ResponseHeaders carry a model call's HTTP headers, observed
	// by the local gateway. A map rather than a typed http.Header because this
	// package must not import net/http semantics into the wire contract.
	RequestHeaders  map[string]string `json:"request_headers,omitempty"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`

	// HTTPMethod/httpurl/HTTPStatus are the classification keys.
	HTTPMethod string `json:"http_method,omitempty"`
	HTTPURL    string `json:"http_url,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`

	// CredentialFingerprint identifies which registered credential made the call,
	// without carrying it. Deliberately NOT gated.
	CredentialFingerprint string `json:"credential_fingerprint,omitempty"`
}

// Content is the only structured location for raw prompt/output/file content.
type Content struct {
	Prompt   string `json:"prompt,omitempty"`
	Output   string `json:"output,omitempty"`
	FileText string `json:"file_text,omitempty"`

	// ToolInput is what the tool was asked to do; the command for a shell call,
	// the arguments for an MCP one; carried so an approver can see what they are
	// deciding about.
	ToolInput string `json:"tool_input,omitempty"`

	// ToolOutput is what the tool produced; the result body of a completed call,
	// or the tool's own error text when the call failed.
	ToolOutput string `json:"tool_output,omitempty"`

	// SignalDetail is a lifecycle signal's free text: why a classifier refused a
	// call (PermissionDenied), what the provider said when a turn failed
	// (APIError).
	SignalDetail string `json:"signal_detail,omitempty"`

	// Thinking is the model's extended-thinking text for a turn; its `thinking`
	// content blocks, concatenated in file order across the turn's transcript
	// window.
	Thinking string `json:"thinking,omitempty"`
}

// DevEvent is the normalized developer-runtime event a caller hands to Emit,
// built from a provider's native payload via the adapter's SPI emit().
type DevEvent struct {
	SchemaVersion string    `json:"schema_version"`
	EventID       string    `json:"event_id"` // client idempotency key (INV-5)
	EventType     EventType `json:"event_type"`
	SessionID     string    `json:"openbox_session_id"`
	DeveloperDID  string    `json:"developer_did"`
	Timestamp     string    `json:"timestamp"` // RFC3339
	StartedAt     string    `json:"started_at,omitempty"`
	EndedAt       string    `json:"ended_at,omitempty"`
	Tool          Tool      `json:"tool"`
	Tokens        *Tokens   `json:"tokens,omitempty"`
	Cost          *Cost     `json:"cost,omitempty"`
	Span          *Span     `json:"span,omitempty"`
	Content       *Content  `json:"content,omitempty"`

	// Status is a tool call's outcome; StatusCompleted or StatusFailed; and is
	// the only thing core reads to decide whether a completed call succeeded.
	// StatusFor enforces both the vocabulary and the event-type scope at the wire
	// boundary, so an adapter mistake cannot widen either.
	Status string `json:"status,omitempty"`

	// Model is the provider model id that spent this event's tokens ("claude-
	// opus-4-8", "gpt-5-codex", …).
	Model string `json:"model,omitempty"`

	// TurnIndex is the zero-based index of the turn this event belongs to within
	// its session (or within its subagent).
	TurnIndex *int `json:"turn_index,omitempty"`

	// AgentID scopes a turn to a subagent. It is set only on turn events fired by
	// SubagentStop, and it partitions the activity id
	// (<session>:agent:<id>:turn:<n>) so a subagent's turns never collide with
	// the main thread's.
	AgentID string `json:"agent_id,omitempty"`

	// SessionRollup marks a turn activity that covers the whole session rather
	// than one turn, giving it activity_id <session_id>:usage:rollup. It is
	// Codex's granularity: its per-turn hook exists but is deliberately unwired,
	// so its usage arrives once, at SessionEnd.
	SessionRollup bool `json:"session_rollup,omitempty"`

	// GatewayRequestID marks a turn event produced by the local gateway rather
	// than by a hook, and supplies that turn's id.
	GatewayRequestID string `json:"gateway_request_id,omitempty"`

	// OtelRequestID marks a turn event produced by the local telemetry receiver;
	// the OTLP intake the governed tool exports to; and supplies that turn's id.
	// Two producers able to mint one id would have half their evidence absorbed
	// as a duplicate; silently, since dedupe is the server behaving correctly.
	OtelRequestID  string `json:"otel_request_id,omitempty"`
	ProxyRequestID string `json:"proxy_request_id,omitempty"`

	// WorkspaceID is a stable per-workspace/developer identity used as core's
	// workflow_id so (workflow_id, run_id) is unique per session (mapping.md §1).
	WorkspaceID string `json:"-"`

	// Metadata carries additive per-type keys (provider, repo, commit_sha, …).
	// Never credentials/secrets (INV-1) or raw content (INV-2).
	Metadata map[string]any `json:"metadata,omitempty"`

	contentStripped bool
}
