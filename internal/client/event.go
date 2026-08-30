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
// (api/dev-event.schema.json v1.0) as Go types, and maps them onto the core
// wire shape per docs/MAPPING.md.
package client

// SchemaVersion is the dev-event contract version this client speaks. Track
// api/dev-event.schema.json's x-schema-version.
//
// 1.1 added the turn pair (ADR-0014) and widened Tokens, re-defining Input as
// pure input. The schema's x-changelog records what changed and why the Tokens
// semantic made it a bump rather than a silent edit.
//
// 1.2 added Status on tool results and the failure/lifecycle event types
// (ADR-0018). Purely additive — every 1.1 event is a valid 1.2 event.
//
// 1.3 added Content.ToolOutput and documented Content.ToolInput, which now also
// rides the OBSERVE ToolCall rather than only a gated call's evaluation event
// (ADR-0019 P1 — the change that retires SL3-SEC-3). Purely additive: both are
// gated fields on an already-gated object, so every 1.2 event is a valid 1.3
// event and an org with content capture off sends byte-identical payloads.
//
// 1.4 added Content.Thinking — the turn's extended-thinking text, in
// activity_output.thinking on a TurnCompleted (ADR-0019 P3, which amends
// ADR-0014's transcript allowlist to permit it). Purely additive and gated the
// same way, so an org with content capture off again sends byte-identical
// payloads.
//
// 1.5 added the LOCAL GATEWAY's span fields (ADR-0021): request/response headers,
// the HTTP classification keys, and credential_fingerprint on Span, plus
// GatewayRequestID on the event. Purely additive — the header pair is gated like
// every other content field and the rest is structural, so a hook-only install
// sends byte-identical payloads and every 1.4 event is a valid 1.5 event.
//
// 1.6 added two model-call producers — OtelRequestID (local telemetry receiver)
// and ProxyRequestID (local transport relay) — and declared SessionRollup, which
// the client had emitted since 1.1 while the schema rejected it. It also repaired
// the contract so BOTH turn halves require exactly one producer discriminator:
// TurnStarted had kept requiring turn_index unconditionally, and 1.5 repaired only
// the close. What that actually broke is the Codex ROLLUP pair, whose opening half
// carries no index — not the gateway, which emits no TurnStarted at all. It would
// equally have broken any later lane emitting a pair (ADR-0022). Additive: no
// existing field moved, no emitted bytes changed, and the shapes that begin to
// validate are ones this client already sent.
const SchemaVersion = "1.6"

// EventType is a developer-runtime lifecycle event type. Each maps 1:1 onto
// an openbox-core event_type string (INV-8) — see MAPPING.md §2.
type EventType string

const (
	EventSessionStarted  EventType = "SessionStarted"
	EventPromptSubmitted EventType = "PromptSubmitted"
	EventToolCall        EventType = "ToolCall"
	EventToolResult      EventType = "ToolResult"
	EventSessionEnded    EventType = "SessionEnded"
	// EventCommitCreated is RESERVED: the wire mapping exists but no adapter
	// produces it. Commit lineage travels via the git trailer and notes into the
	// Deploy event's metadata.
	EventCommitCreated EventType = "CommitCreated"
	EventDeploy        EventType = "Deploy"

	// EventTurnStarted / EventTurnCompleted are one model turn's boundaries:
	// the unit a coding agent spends tokens in. They ride the same activity
	// carrier as a tool call (ActivityStarted/ActivityCompleted with
	// activity_type "llm_completion"), because a dev session writes no spans
	// (ADR-0013) and the AI-Agent runtime's equivalent signal lives in an
	// llm_completion span's response_body. Same shape, different carrier — see
	// ADR-0014.
	//
	// Both halves are emitted from ONE provider hook firing (Claude Code's
	// Stop), so the pair is atomic: there is no cross-hook index to race and no
	// orphan half. They share one activity_id derived from TurnIndex
	// (turnActivityIDFor), which is what pairs them onto one row.
	EventTurnStarted   EventType = "TurnStarted"
	EventTurnCompleted EventType = "TurnCompleted"

	// The failure/lifecycle signals (ADR-0018). All three ride stock
	// SignalReceived (INV-8) — no new endpoint, no new table, per the repo's
	// reuse rule — and all three are metadata-only.
	//
	// They carry NO signal_args, and that is a correctness constraint rather
	// than a minimalism preference. Core's goal-alignment engine treats ANY
	// SignalReceived with non-empty signal_args as a new user goal: it runs an
	// alignment check against the assistant messages accumulated so far and then
	// OVERWRITES the session's goal with the stringified args
	// (openbox-core internal/services/age.go:112-137). Putting the denied tool's
	// name in signal_args would therefore replace the developer's actual prompt
	// as the thing every later turn is scored against, silently wrecking the
	// feature the turn span exists to feed. Structural detail rides metadata.
	// TestNewSignalsCarryNoSignalArgs holds this.

	// EventSubagentStarted marks a subagent spawning. Until this existed a
	// subagent was visible only through the agent_id on its tool events, so one
	// that spawned and did nothing left no trace.
	EventSubagentStarted EventType = "SubagentStarted"
	// EventPermissionDenied records that a policy or classifier refused a tool
	// call — that a decision happened, which tool it was about, and, under the
	// content gate, why (ADR-0019 P1: the provider's free-text `reason` rides
	// Content.SignalDetail → metadata.denial_reason). Never the tool's content.
	EventPermissionDenied EventType = "PermissionDenied"
	// EventAPIError records a turn that ended in a provider-side error rather
	// than an answer (rate limit, billing, auth, overload). Without it, a
	// session throttled into silence is indistinguishable from an idle one.
	EventAPIError EventType = "APIError"
)

// AllEventTypes is the complete vocabulary, so callers that need to enumerate it
// (contract cross-checks, test fixtures, coverage bookkeeping) can read it from
// the constants instead of re-typing the list. The vocabulary was previously
// declared in four places with nothing binding them together, and the only
// cross-check compared list lengths — which a rename passes unchanged.
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

// Tool-result outcome vocabulary (ADR-0018 Decision 1). Two literals, closed.
//
// The strings are not ours to choose: core compares the wire value against the
// literal "completed" and nothing else
// (openbox-core internal/services/activities/observability/errors.go:333). A
// near-miss — "success", "COMPLETED", "complete" — does not degrade the metric,
// it pins it at 0%, which is exactly the state this field exists to fix. So the
// vocabulary is closed and statusFor drops anything outside it rather than
// forwarding a value core will silently score as a failure.
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
//
// Input is PURE input: prompt tokens that were neither served from nor written
// to the provider's prompt cache. The two cache counts ride their own fields.
// This is a change of semantic — the Claude Code SessionEnd rollup used to fold
// both cache counts into Input because there was nowhere else to put them, which
// made a cached-heavy session look like it had spent its whole context on fresh
// input and made cache efficiency unmeasurable. Total keeps its old meaning:
// whole throughput, Input + Output + both cache counts.
//
// Every field is a pointer so "absent" and "zero" stay distinguishable: a
// provider that does not report cache tokens omits them rather than claiming
// zero.
type Tokens struct {
	Input  *int `json:"input,omitempty"`
	Output *int `json:"output,omitempty"`
	Total  *int `json:"total,omitempty"`
	// CacheCreationInput is input tokens WRITTEN to the prompt cache
	// (Anthropic cache_creation_input_tokens; Codex cache_write_input_tokens).
	CacheCreationInput *int `json:"cache_creation_input,omitempty"`
	// CacheRead is input tokens SERVED from the prompt cache (Anthropic
	// cache_read_input_tokens; Codex cached_input_tokens).
	CacheRead *int `json:"cache_read,omitempty"`
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

	// InvocationID and OperationID separate the two identities a tool call has.
	// Both are LOCAL: they are persisted in the spool (so the enforce path and
	// the later flush derive the same ids) but are never emitted as wire fields
	// — they exist only to feed the opaque activity_id hash and the duration
	// stash key.
	//
	// InvocationID is THIS attempt (the provider's tool_use_id). It keys the
	// cross-process duration stash, so a PostToolUse hook can recover when its
	// PreToolUse fired. The two halves pair on the wire by activity_id.
	//
	// OperationID is WHAT is being done, and must be identical across a retry
	// of the same operation. It is the load-bearing one: activity_id is derived
	// from it, activity_id is the approval key, and core scopes BOTH of its
	// bypass grants by activity_id. Keying that on the invocation instead —
	// which is what carrying tool_use_id in Function used to do — means an
	// approved request can never be consumed: the retry addresses a different
	// record, files a fresh approval, and asks the human again. See
	// OperationForCommand / OperationForArgs for how it is derived.
	InvocationID string `json:"invocation_id,omitempty"`
	OperationID  string `json:"operation_id,omitempty"`

	// RequestBody/ResponseBody are gated content (INV-2): stripped before
	// egress unless content-capture is enabled for the org.
	RequestBody  string `json:"request_body,omitempty"`
	ResponseBody string `json:"response_body,omitempty"`

	// RequestHeaders/ResponseHeaders carry a model call's HTTP headers, observed
	// by the local gateway (ADR-0021). GATED content, and the highest-risk class
	// this client has: the developer's live provider credential is on every
	// request. Two mechanisms stand between it and the wire and BOTH are
	// mandatory — the capture side redacts by key name before these are ever
	// populated, and stripContent empties them here when the org opted out.
	//
	// Header values are already-redacted strings, joined per key. A map rather
	// than a typed http.Header because this package must not import net/http
	// semantics into the wire contract.
	RequestHeaders  map[string]string `json:"request_headers,omitempty"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`

	// HTTPMethod/HTTPURL/HTTPStatus are the classification keys. Core RECOMPUTES
	// semantic_type per span and isLLMCall reads http.method plus an LLM domain
	// in http.url, so a gateway span without these stores as something else and
	// alignment silently dies — the same trap ADR-0018's synthesized attributes
	// documented. Structural, not content: a method, a status and a URL whose
	// query is dropped at capture.
	HTTPMethod string `json:"http_method,omitempty"`
	HTTPURL    string `json:"http_url,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`

	// CredentialFingerprint identifies WHICH registered credential made the call,
	// without carrying it. One-way SHA-256 over the raw header value, truncated,
	// computed at capture BEFORE the key-name redaction that removes the value.
	//
	// Deliberately NOT gated. It is derived evidence like Status — but unlike
	// Status it derives FROM a secret, so "the raw value is absent while this is
	// present" is asserted on outbound bytes rather than assumed. Account binding
	// is a governance control; making it disappear under a privacy setting would
	// let an org opt out of being identified.
	CredentialFingerprint string `json:"credential_fingerprint,omitempty"`
}

// Content is the only structured location for raw prompt/output/file
// content. Gated (INV-2): stripped before egress unless content-capture is
// enabled.
type Content struct {
	Prompt   string `json:"prompt,omitempty"`
	Output   string `json:"output,omitempty"`
	FileText string `json:"file_text,omitempty"`

	// ToolInput is what the tool was asked to do — the command for a shell
	// call, the arguments for an MCP one — carried so an approver can see what
	// they are deciding about.
	//
	// It is set on an inline evaluation — where the adapter rebuilds it from
	// the enforce gate's own detection result, so the server judges the exact
	// bytes the tool call was rewritten to — AND, since ADR-0019 P1, on the
	// observe path under the same content gate.
	//
	// This comment used to say it was "never set on the observe path, so
	// SL3-SEC-3 is unchanged". That guarantee is retired: ordinary tool
	// telemetry does gain the command now, when the org has content capture on.
	//
	// Why it exists: an approval request that reads `kind=shell
	// tool_name=Bash` is not decidable. Neither a human nor an autonomous
	// approver can act on it, so the gate manufactures an audit trail
	// asserting a control ran while the control was decorative — the exact
	// failure the design warns about. An org that turned on Tier-2 AND wrote a
	// require_approval policy has asked to be shown these calls; showing the
	// tool's name alone does not answer the question it asked.
	//
	// It is content, and gated like all content: with content capture off,
	// stripContent drops it at the client choke point and the approver sees
	// only the structural fields — a real posture choice with a real cost,
	// recorded as OD-E9-7.
	ToolInput string `json:"tool_input,omitempty"`

	// ToolOutput is what the tool PRODUCED — the result body of a completed
	// call, or the tool's own error text when the call failed. It lands in
	// `activity_output.output`, the field core stores as the row's `output` and
	// runs Guardrails stage "1" over.
	//
	// It is a separate field from Output rather than a reuse of it, and the
	// distinction is load-bearing: Output carries the assistant's turn text
	// (ADR-0018), which rides a TurnCompleted's span. Overloading one field
	// across both would put turn text on tool events and tool output in the
	// alignment extractor the moment either mapping slipped.
	//
	// Gated like every other Content field — stripContent nils Content when the
	// org has content capture off — redacted for secrets by the adapter BEFORE
	// attachment, and capped at maxBodySize by structuralActivityOutput before
	// the payload is signed. With `secret_detection:false` it egresses
	// unredacted; stated, not mitigated.
	ToolOutput string `json:"tool_output,omitempty"`

	// SignalDetail is a lifecycle signal's free text: why a classifier refused a
	// call (PermissionDenied), what the provider said when a turn failed
	// (APIError). It lands in `metadata`, under a key named per event type —
	// beside `error_type` on an APIError, beside `tool_name`/`tool_use_id` on a
	// PermissionDenied — because metadata is where a signal's structural detail
	// already rides (MAPPING.md §2).
	//
	// It emphatically does NOT land in `signal_args`, and that is a correctness
	// constraint, not a stylistic one: core's alignment engine reads ANY
	// SignalReceived with non-empty signal_args as a NEW USER GOAL and
	// overwrites the session's goal with the stringified args
	// (openbox-core internal/services/age.go:112-137). A denial reason routed
	// there would replace the developer's prompt as the thing every later turn
	// is scored against, and the symptom would look like drift rather than a
	// bug. Conformance C38 holds it.
	//
	// It is a Content field rather than an adapter-set metadata key so the gate
	// is a property of the choke point: stripContent nils Content, so no
	// mis-typed key can route free text around the posture.
	SignalDetail string `json:"signal_detail,omitempty"`

	// Thinking is the model's extended-thinking text for a turn — its
	// `thinking` content blocks, concatenated in file order across the turn's
	// transcript window. It lands in `activity_output.thinking` on a
	// TurnCompleted (v1.4, ADR-0019 P3 / the ADR-0014 amendment).
	//
	// It is a separate field from Output for the same reason ToolOutput is:
	// Output is the assistant's ANSWER and rides the one span core's alignment
	// extractor reads. Chain-of-thought in that span would make every later
	// turn's drift score compare against the model's reasoning instead of its
	// reply — a silent corruption of the reader, not a formatting choice.
	//
	// The transcript is the only source: no hook payload carries thinking, and
	// Claude Code's own OTel export redacts it unconditionally with every
	// content flag enabled. Capturing it therefore goes FURTHER than the
	// provider will, on the org's own machine, by the org's own decision —
	// recorded in the ADR-0014 amendment rather than inferred from "capture
	// everything".
	//
	// It is also the densest content this client carries: thinking restates
	// prompts, file bodies, and any credential the turn saw earlier. Gate,
	// redact, cap — in that order, all three asserted on outbound bytes. With
	// `secret_detection:false` it egresses unredacted; stated, not mitigated.
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

	// Status is a tool call's outcome — StatusCompleted or StatusFailed — and
	// is the only thing core reads to decide whether a completed call succeeded.
	// Set on ToolResult and nowhere else: payload.Status also writes the row's
	// workflow_status column for ANY event type
	// (openbox-core .../governance/storage_event.go:417), so putting a tool
	// outcome on a lifecycle event would overwrite a genuinely workflow-scoped
	// field. statusFor enforces both the vocabulary and the event-type scope at
	// the wire boundary, so an adapter mistake cannot widen either.
	//
	// Structural, NOT content (INV-2): it is derived from which hook fired, or
	// from a bound bool/int — never parsed out of tool output. It is therefore
	// not content-gated and ships identically with content_capture off. A
	// two-literal enum has no room to encode anything.
	Status string `json:"status,omitempty"`

	// Model is the provider model id that spent this event's tokens
	// ("claude-opus-4-8", "gpt-5-codex", …). It is the ONE free-form string the
	// transcript/rollout projections egress, and it is load-bearing rather than
	// decorative: the backend aggregates token rollups under model-keyed
	// composite metric keys, so a turn without a model is invisible to the
	// dashboards it is meant to feed.
	//
	// Identifier-class, not content: bounded at the adapter boundary (capStr)
	// because it is provider-controlled text. It feeds both the
	// llm_completion activity_output and metadata.model. Absent when the
	// projection found none — never back-filled from a session-level model,
	// which would attribute tokens to a model that may not have spent them.
	Model string `json:"model,omitempty"`

	// TurnIndex is the zero-based index of the turn this event belongs to
	// within its session (or within its subagent). Both halves of a turn pair
	// carry the same value, which is what makes turnActivityIDFor derive one
	// activity_id for the pair.
	//
	// A pointer so index 0 — the first turn of every session — is
	// distinguishable from "not a turn event". It survives the spool round-trip
	// (an exported, JSON-tagged field), so a rehydrated flush derives the same
	// id the enforce path would have.
	TurnIndex *int `json:"turn_index,omitempty"`

	// AgentID scopes a turn to a subagent. It is set only on turn events fired
	// by SubagentStop, and it partitions the activity id
	// (<session>:agent:<id>:turn:<n>) so a subagent's turns never collide with
	// the main thread's. Empty on the main thread. Structural identifier
	// (INV-2); the same value also rides metadata.agent_id.
	AgentID string `json:"agent_id,omitempty"`

	// SessionRollup marks a turn activity that covers the WHOLE SESSION rather
	// than one turn, giving it activity_id <session_id>:usage:rollup. It is
	// Codex's granularity: its per-turn hook exists but is deliberately unwired
	// (ADR-0014), so its usage arrives once, at SessionEnd.
	//
	// It is an explicit flag rather than "a turn event with no TurnIndex"
	// deliberately. Inferring it from an absent index would turn a Claude Code
	// bug — an index that failed to be set — into a silent collapse of every
	// turn in the session onto one activity_id, which core would then dedupe
	// down to a single row. An indexless, non-rollup turn stays what it is: a
	// defect, caught by the pin test.
	SessionRollup bool `json:"session_rollup,omitempty"`

	// GatewayRequestID marks a turn event produced by the LOCAL GATEWAY rather
	// than by a hook, and supplies that turn's id (ADR-0021).
	//
	// It exists to keep the producers' activity ids in disjoint namespaces, which
	// is the whole of requirement 8. It was two producers when this field was
	// added and is five since ADR-0022 (see OtelRequestID below); the argument did
	// not change, only its arity. Each describes the same model turn from a
	// different vantage point; if any two could mint the same activity_id, core's
	// dedupe — keyed on (agent_id, workflow_id, run_id, activity_id, event_type)
	// — would absorb one as a duplicate of the other and silently drop half the
	// evidence.
	//
	// The id is opaque to core. It has to be derivable from fields that survive
	// the spool, for the same reason TurnIndex does: a flush can happen long
	// after the process that built the event exited.
	GatewayRequestID string `json:"gateway_request_id,omitempty"`

	// OtelRequestID marks a turn event produced by the LOCAL TELEMETRY RECEIVER
	// — the OTLP intake the governed tool exports to — and supplies that turn's
	// id (ADR-0022). ProxyRequestID does the same for the LOCAL TRANSPORT RELAY,
	// which observes the call in path rather than being told about it.
	//
	// They exist for exactly the reason GatewayRequestID does, and the reason
	// scales with each lane added: five producers now describe the same model
	// turn from different vantage points, and core's dedupe key is
	// (agent_id, workflow_id, run_id, activity_id, event_type). Two producers
	// able to mint one id would have half their evidence absorbed as a duplicate
	// — silently, since dedupe is the server behaving correctly. The namespaces
	// make the ids disjoint; the producer election (one lane per session) makes
	// the COUNT right. Both are needed: disjoint ids still double-report a turn.
	//
	// Both values originate upstream — a provider request id relayed through an
	// OTLP payload, or read off a relayed response — and reach a stored key
	// verbatim, so each producer bounds and charset-checks its own before
	// setting it (gatewayemit.usableRequestID is the shape), and the contract
	// states the same bound declaratively so a later lane inherits it.
	//
	// Structural identifiers (INV-2), never derived from prompt or body text,
	// and therefore not content-gated: neither joins contentMetadataKeys.
	OtelRequestID  string `json:"otel_request_id,omitempty"`
	ProxyRequestID string `json:"proxy_request_id,omitempty"`

	// WorkspaceID is a stable per-workspace/developer identity used as core's
	// workflow_id so (workflow_id, run_id) is unique per session (MAPPING.md
	// §1). Defaults to DeveloperDID when empty.
	WorkspaceID string `json:"-"`

	// Metadata carries additive per-type keys (provider, repo, commit_sha, …).
	// Never credentials/secrets (INV-1) or raw content (INV-2).
	Metadata map[string]any `json:"metadata,omitempty"`

	// contentStripped records that Emit removed gated content from this event
	// because content capture is off. buildMetadata reads it to drop
	// content-bearing metadata keys too, so INV-2 holds at the choke point
	// rather than resting on every adapter getting it right.
	contentStripped bool
}
