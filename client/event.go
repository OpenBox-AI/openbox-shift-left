// Package client is the OpenBox developer-runtime data-plane client: the
// shared transport every adapter and the git action use to emit a normalized
// developer event to OpenBox — build the openbox-core GovernanceEventPayload,
// AIP Ed25519-sign the request, POST it to /api/v1/governance/evaluate, and
// parse the verdict.
//
// Design constraints:
//   - INV-1: the obx_ API key and the Ed25519 signing seed are never logged
//     or placed on an argv; they live only in the Client and request headers.
//   - INV-2: content (prompt/output/file/tool bodies) is stripped before
//     egress unless content-capture is explicitly enabled for the org.
//   - INV-3: fail-open. A transport failure logs and drops (or buffers) the
//     event; Emit never blocks or errors the caller in observe mode.
//   - INV-5: each event carries a client-generated idempotency id (EventID)
//     so retries and buffered flushes are never double-counted by core.
//
// This package mirrors the normalized event contract
// (contracts/dev-event/schema/dev-event.schema.json v1.0) as Go types, and
// maps them onto the core wire shape per contracts/dev-event/MAPPING.md.
package client

// SchemaVersion is the dev-event contract version this client speaks. Track
// contracts/dev-event/schema/dev-event.schema.json's x-schema-version.
const SchemaVersion = "1.0"

// EventType is a developer-runtime lifecycle event type. Each maps 1:1 onto
// an openbox-core event_type string (INV-8) — see MAPPING.md §2.
type EventType string

const (
	EventSessionStarted  EventType = "SessionStarted"
	EventPromptSubmitted EventType = "PromptSubmitted"
	EventToolCall        EventType = "ToolCall"
	EventToolResult      EventType = "ToolResult"
	EventSessionEnded    EventType = "SessionEnded"
	EventCommitCreated   EventType = "CommitCreated"
	EventDeploy          EventType = "Deploy"
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

// Tokens is per-turn/per-tool token usage (metadata only; absent when unknown).
type Tokens struct {
	Input  *int `json:"input,omitempty"`
	Output *int `json:"output,omitempty"`
	Total  *int `json:"total,omitempty"`
}

// Cost is per-turn/per-tool monetary cost (metadata only; absent when unknown).
type Cost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency,omitempty"`
}

// Span is one semantic span for a tool call/result. It mirrors the subset of
// openbox-core SpanData an adapter sets; core computes semantic_type
// server-side from these source fields (MAPPING.md §3).
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

	// RequestBody/ResponseBody are gated content (INV-2): stripped before
	// egress unless content-capture is enabled for the org.
	RequestBody  string `json:"request_body,omitempty"`
	ResponseBody string `json:"response_body,omitempty"`
}

// Content is the only structured location for raw prompt/output/file
// content. Gated (INV-2): stripped before egress unless content-capture is
// enabled.
type Content struct {
	Prompt   string `json:"prompt,omitempty"`
	Output   string `json:"output,omitempty"`
	FileText string `json:"file_text,omitempty"`
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

	// WorkspaceID is a stable per-workspace/developer identity used as core's
	// workflow_id so (workflow_id, run_id) is unique per session (MAPPING.md
	// §1). Defaults to DeveloperDID when empty.
	WorkspaceID string `json:"-"`

	// Metadata carries additive per-type keys (provider, repo, commit_sha, …).
	// Never credentials/secrets (INV-1) or raw content (INV-2).
	Metadata map[string]any `json:"metadata,omitempty"`
}
