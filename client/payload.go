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

// source tags developer-runtime traffic on the wire. core's `source` field is
// free-form/unvalidated (MAPPING.md §6); this distinguishes developer events
// from the SDK's "workflow-telemetry".
const source = "developer-runtime"

// governanceEventPayload mirrors the subset of openbox-core's
// GovernanceEventPayload (internal/content/governance.go:186) that the
// developer-runtime client sets. Fields core populates for Temporal events
// (task_queue, parent_workflow_id, attempt, …) are intentionally omitted —
// they stay absent (omitempty), which is additive and INV-8-safe.
//
// One struct serializes every developer event. There is no second, map-shaped
// regime: tool calls are activity events like everything else (ADR: tool call
// as activity), so field order on the wire is this declaration order, for all
// of them.
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
	// scopes its bypass grants by it. Set on tool events only — see
	// activityIDFor for why it must stay operation-derived.
	ActivityID string `json:"activity_id,omitempty"`
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id"`
	// WorkflowType is the base wire contract's required workflow discriminator.
	// Kept constant per session (workflowType) so WorkflowStarted, its
	// SignalReceived events, its activity events and WorkflowCompleted all
	// resolve to one workflow.
	WorkflowType string `json:"workflow_type,omitempty"`
	// SignalName is required on a SignalReceived event, empty on
	// Workflow*/Activity* events.
	SignalName string `json:"signal_name,omitempty"`
	// SignalArgs carries a SignalReceived event's arguments (the openbox-fe
	// Verify-tab "Input" detail reads log.signal_args). Commit/deploy signals
	// carry structural lineage identifiers (commit_sha/repo/deploy_id/…); a
	// prompt_submitted signal carries the prompt only under content-capture
	// (content — INV-2 — gated like a request_body, capped, absent by
	// default), never a commit-message body or session context. See
	// buildSignalArgs.
	SignalArgs json.RawMessage `json:"signal_args,omitempty"`
	// ActivityInput rides ActivityStarted; core stores it as the row's `input`
	// and runs Guardrails stage "0" over it (services/guardrail.go:180).
	// Structural only, plus the content-gated approval context — see
	// structuralActivityInput.
	ActivityInput json.RawMessage `json:"activity_input,omitempty"`
	// ActivityOutput rides ActivityCompleted; core stores it as the row's
	// `output` and runs Guardrails stage "1" over it
	// (services/guardrail.go:192). Counts and an exit code, plus — under the
	// content gate — the tool's own output text (ADR-0019 P1; on a failed call
	// that text is the tool's error). See structuralActivityOutput.
	ActivityOutput json.RawMessage `json:"activity_output,omitempty"`
	// DurationMs is how long the tool call took, in milliseconds. The client
	// computes it because there is no longer a span for core to derive it from:
	// core copies this field straight onto the row
	// (activities/governance/storage_event.go:292-294) and the dashboard reads
	// event.duration_ms directly. Absent rather than zero when unknown — see
	// durationMs.
	DurationMs *float64        `json:"duration_ms,omitempty"`
	Timestamp  string          `json:"timestamp"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	// Status is the tool call's outcome, and the single field core's per-tool
	// success metric reads:
	//
	//	metric.IsSuccess = payload.Status != nil && *payload.Status == "completed"
	//	  — openbox-core internal/services/activities/observability/errors.go:333
	//
	// It went unwritten by every producer for this field's whole existence, while
	// `.total` incremented on every ActivityStarted — so each completion scored
	// as tool.<name>.failed and SUCCESS read 0.0% by construction (ADR-0018).
	//
	// APPENDED LAST deliberately. Key order on the wire is this struct's
	// declaration order and the golden fixtures pin it byte-exactly; adding the
	// field anywhere else would rewrite every fixture and obscure the one-key
	// diff that shows this change is additive.
	Status string `json:"status,omitempty"`
	// Spans carries EXACTLY ONE span, on a TurnCompleted under content capture,
	// and nothing else ever (ADR-0018 Decision 2). It is not a return of the
	// span layer ADR-0013 retired: tool events stay span-less and the deleted
	// files stay deleted. It exists because core's goal-alignment extractor
	// reads assistant text from payload.Spans and from no other field, so a
	// span-less session can never feed it — see client/turnspan.go.
	//
	// Both keys are absent unless there is text, which is what makes
	// content_capture:false emit nothing new at all rather than an empty array.
	Spans     []wireSpan `json:"spans,omitempty"`
	SpanCount int        `json:"span_count,omitempty"`
}

// Base wire event types (INV-8: every dev event maps onto one of these stock
// types, so a stock core accepts it with no patch). All five are on core's
// accept-list (internal/api/governance.go:273-286).
const (
	wireWorkflowStarted   = "WorkflowStarted"
	wireWorkflowCompleted = "WorkflowCompleted"
	wireSignalReceived    = "SignalReceived"
	wireActivityStarted   = "ActivityStarted"
	wireActivityCompleted = "ActivityCompleted"
)

// buildPayload maps a normalized DevEvent onto core's GovernanceEventPayload
// and marshals it to the exact bytes that will be signed and transmitted.
// Content-stripping (INV-2) has already run in Emit when content-capture is
// disabled, so any content still present here is authorized.
//
// Every developer event takes one path onto stock accept-listed base wire types
// (INV-8):
//   - SessionStarted/Ended → Workflow* (session = workflow)
//   - PromptSubmitted/CommitCreated/Deploy → SignalReceived(signal_name)
//   - ToolCall → ActivityStarted, ToolResult → ActivityCompleted
//
// A tool execution IS an activity: it is the unit of work a developer session
// performs, it is what an approver decides about, and both halves are evaluated
// independently by core. Tool events used to ride an ActivityStarted+hook_trigger
// envelope carrying a hand-fabricated OTel span, because the base SDK reserves
// ActivityCompleted for hook-less lifecycle events. That rule binds runtimes
// that HAVE in-process OTel to produce a span with; a hook process has none, so
// the span was invented to satisfy a shape rather than to record a measurement.
// Modelling the call at the activity layer instead retires the span layer whole
// — see the ADR for what that costs (no span rows, no span-level Merkle leaves,
// no server-side semantic_type for dev sessions).
//
// The bytes returned here are BOTH hashed for the signature AND sent as the
// body, so they are produced exactly once — client.go never re-marshals. Key
// order is this struct's declaration order and the golden fixtures pin it.
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

	// Activity fields ride tool events only. activity_id is set on BOTH halves
	// — it is what pairs them onto one row and what an approval addresses —
	// while input rides the started half and output/duration the completed one,
	// matching the stage Guardrails evaluates each against.
	switch ev.EventType {
	case EventToolCall:
		p.ActivityID = activityIDFor(ev)
		p.ActivityInput = structuralActivityInput(ev)
	case EventToolResult:
		p.ActivityID = activityIDFor(ev)
		p.ActivityOutput = structuralActivityOutput(ev)
		p.DurationMs = durationMs(ev)
		p.Status = statusFor(ev)
	// A turn is an activity too (ADR-0014). Its id is derived from the turn
	// index rather than hashed from an operation, because a turn has no
	// operation to key on and a readable id is worth having in stored rows.
	// The Started half carries no activity_input: the input to a turn is the
	// prompt, which is content, and the PromptSubmitted signal already carries
	// it under the content gate.
	case EventTurnStarted:
		p.ActivityID = turnActivityIDFor(ev)
	case EventTurnCompleted:
		p.ActivityID = turnActivityIDFor(ev)
		p.ActivityOutput = turnActivityOutput(ev)
		p.DurationMs = durationMs(ev)
		// The assistant's words, when capture left them on the event. Note what
		// is deliberately NOT set alongside: hook_trigger. A payload with
		// hook_trigger true AND spans present enters core's approval-bypass
		// fingerprint path (governance_workflow.go:310-330), and a model turn is
		// not an approvable operation.
		if span := turnAssistantSpan(ev); span != nil {
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

// workflowType is the base wire contract's required `workflow_type` for a
// developer session. It's a constant (not the provider name — that rides in
// metadata) so a session's WorkflowStarted, every SignalReceived and activity
// event it carries, and its WorkflowCompleted share one (workflow_id, run_id,
// workflow_type) identity and Core resolves them to the same workflow/session
// row.
const workflowType = "developer-session"

// wireTypeFor maps a developer-runtime EventType onto its base wire event_type
// and, for a signal, its signal_name. The DevEvent EventType is preserved as the
// dashboard activity_type label (activityLabel); only the wire type is
// rewritten.
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
	// The failure/lifecycle signals (ADR-0018). Stock SignalReceived, so a stock
	// core accepts them with no patch (INV-8). buildSignalArgs deliberately has
	// no arm for any of them — see the EventSubagentStarted doc comment for why
	// non-empty signal_args on these would overwrite the goal-alignment session's
	// user goal.
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
	// Emitting the DevEvent's own string here produced a non-accept-listed
	// event_type: a guaranteed 400 that the fail-open path then swallowed, so a
	// new event type would go silently undelivered. Failing to build says so.
	return "", "", fmt.Errorf("client: no base wire type for event_type %q", et)
}

// activityPairKey identifies the OPERATION a tool call performs: the session,
// the tool, its structural locator, and the operation discriminator the adapter
// derived (see Span.OperationID). It excludes the stage and the timestamp — the
// fields that differ between a call's paired started/completed events — and
// every field survives the spool round-trip, so the derived ids are stable even
// after a rehydrated flush.
//
// It must be identical across a RETRY of the same operation, because
// activity_id is derived from it, activity_id is the approval key, and core
// scopes both of its bypass grants by activity_id. It used to fold in the
// provider's per-invocation tool_use_id (carried in Span.Function), which made
// every retry a different activity: the approval an approver had granted could
// never be consumed, the retry filed a fresh request, and a rewake that said
// "re-run to proceed" looped indefinitely, burning one human decision per pass.
//
// No content feeds the key (INV-2). The file path and function are structural
// locators, and the operation discriminator is a hash — see client/operation.go
// for why that is a correlation id rather than a content field.
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
// to the stable per-developer one. Every event uses it, including the tool
// events, so a session's whole tree resolves to one workflow.
//
// It is derived in exactly one place because ApprovalKeyFor calls it too: a
// poll built from an independently-computed id would address a different row
// than the escalation created, and the hold would report "never decided" for an
// approval that was in fact granted.
func workflowIDFor(ev DevEvent) string {
	if ev.WorkspaceID != "" {
		return ev.WorkspaceID
	}
	return ev.DeveloperDID
}

// activityIDFor is the wire activity_id (free-form; not hex-constrained): the
// pairing key shared by a tool call's ActivityStarted and ActivityCompleted, and
// the approval key ApprovalKeyFor addresses. Same single-derivation rule as
// workflowIDFor, for the same reason.
func activityIDFor(ev DevEvent) string {
	sum := sha256.Sum256([]byte("act\x1f" + activityPairKey(ev)))
	return "cc-act-" + hex.EncodeToString(sum[:16])
}

// turnActivityIDFor is the wire activity_id shared by a turn's ActivityStarted
// and ActivityCompleted: "<session_id>:turn:<index>", or
// "<session_id>:agent:<agent_id>:turn:<index>" for a subagent's turn.
//
// It is a DIFFERENT derivation from activityIDFor deliberately, on three counts:
//
//   - There is nothing to hash. A tool call's id hashes an operation so that a
//     retry of the same operation addresses the approval already granted for it.
//     A turn is not retried and is never approved, so a hash would only make the
//     id unreadable in stored rows for no gain.
//   - It must be derivable from fields that survive the spool (SessionID,
//     TurnIndex, AgentID are all persisted), because a flush can happen long
//     after the hook process that built the event exited.
//   - It cannot collide with a tool-call id by construction: those are
//     "cc-act-" + 32 hex chars, and this shape contains ':' and a decimal index.
//
// Core treats activity_id as an opaque string and its dedupe key is
// (agent_id, workflow_id, run_id, activity_id, event_type) — so re-emitting a
// turn after a crash re-mints this exact id and the server absorbs it rather
// than storing a second row. That is what makes the cursor's
// over-report-on-crash direction safe.
//
// A SessionRollup turn — Codex's granularity, one usage activity per session —
// takes "<session_id>:usage:rollup" instead, which cannot collide with an indexed
// turn because "rollup" is not a decimal number.
//
// Returns "" when TurnIndex is unset on a non-rollup turn, which keeps the field
// omitted rather than minting "<session>:turn:" for something that is not a turn.
// TestTurnActivityIDIsPinned holds these bytes.
func turnActivityIDFor(ev DevEvent) string {
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

// activityTypeLLMCompletion is the activity_type both turn halves carry. The
// name is not invented here: core already uses "llm_completion" as a semantic
// type for the AI-Agent runtime's model-call spans
// (openbox-core internal/content/session.go:105), so one vocabulary spans both
// runtimes and the core-side extractor keys on a name it already knows.
const activityTypeLLMCompletion = "llm_completion"

// turnActivityOutput builds the `activity_output` for a turn's
// ActivityCompleted: the model that ran and the four token counts it spent.
//
// The shape mirrors the AI-Agent llm_completion span's response_body
// ({model, usage{…}}) so a consumer reads one shape regardless of which runtime
// produced it — which is the whole point of routing the turn through an activity
// instead of reviving the span layer ADR-0013 retired.
//
// INV-2, stated exactly: THIS OBJECT carries FOUR NUMBERS AND ONE IDENTIFIER.
// No prompt, no completion, no thinking block, no stop reason, no tool content.
// The model id is the single free-form string, already capStr-bounded by the
// adapter. Core runs Guardrails stage "1" and OPA over this field, so token
// spend becomes policy-visible — an intended upside, and a second reason the
// schema must stay numbers plus one bounded identifier.
//
// Scope note (ADR-0018): the sentence above is about activity_output and stays
// exactly true. The TURN EVENT as a whole is no longer content-free — under
// content capture it carries the assistant's text, on the span
// (buildPayload's EventTurnCompleted arm, client/turnspan.go). The text was put
// there rather than here for one reason: core's alignment extractor reads
// payload.Spans and nothing else. openbox-core#130 asks for it to read this
// field instead, and when that lands the text moves HERE as
// activity_output.message and the span is deleted — at which point this
// paragraph is what needs rewriting.
//
// Cost is deliberately absent. Core and the backend each derive it server-side
// from a model-keyed pricing table; deriving it here would fabricate a number
// from a table this client has no business owning.
//
// Returns nil (field omitted) when the turn carried neither usage nor a model.
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
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// toolStatuses is the closed wire vocabulary for `status` (ADR-0018).
var toolStatuses = map[string]bool{
	StatusCompleted: true,
	StatusFailed:    true,
}

// statusFor resolves the wire `status` for an event, enforcing BOTH halves of
// the field's contract at the one boundary every event crosses:
//
//   - event-type scope — tool results only. A turn already fails core's tool
//     metric by exclusion (errors.go:320-322), but a lifecycle event would land
//     its value in governance_events.workflow_status, a column that means
//     something else. That is the binding reason, and it is why this is checked
//     here rather than trusted to each adapter.
//   - vocabulary — anything but the two literals is DROPPED, not forwarded.
//     Core scores every non-"completed" value as a failure, so shipping a typo
//     would report 0% success just as convincingly as shipping nothing, while
//     looking correct in the payload. Omitting says "unknown", which is true.
//
// Returns "" (the field is omitted) for every other event and every unknown
// value.
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
// pass-through `activity_type` column (openbox-fe's "Activity" column reads
// it first and shows "Unknown" when absent). It's derived only from fields
// that survive the adapter's spool round-trip (EventType + Tool.Name are
// persisted; a `json:"-"` field would not), so a spooled tool call still
// lands its specific tool name:
//   - a tool event (ToolCall/ToolResult) → the specific tool name ("Edit"/
//     "Bash"/"mcp__…"), the most useful Activity label;
//   - a turn event → "llm_completion", the same label on BOTH halves, so the
//     core-side usage extractor has one key to select on and the two runtimes
//     share one vocabulary;
//   - everything else (lifecycle, Deploy) → the event_type string.
//
// Always non-empty. Identifier-class only — a tool name, a fixed label, or an
// event type — never content (INV-2).
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
// contentMetadataKeys are metadata keys that carry free text rather than a
// structural identifier. Adapters are not supposed to put content in metadata,
// but nothing stopped them: stripContent nulls Content and the span bodies,
// while buildMetadata copied every adapter-supplied key through untouched. That
// made INV-2 a convention observed by every adapter rather than a property of
// the one choke point every event passes through.
//
// A key here is dropped when content capture is off, exactly like Content. The
// list is deliberately small and specific — a backstop against a mistake, not a
// content classifier.
var contentMetadataKeys = map[string]bool{
	"message":   true, // a commit message body
	"prompt":    true,
	"output":    true,
	"content":   true,
	"file_text": true,
	"diff":      true,
	"patch":     true,
	"body":      true,
	"stdout":    true,
	"stderr":    true,
	// Tool input DOES egress on the observe path since ADR-0019 P1 — but under
	// Content.ToolInput, which stripContent nils. This key stays listed as the
	// backstop it always was: an adapter that put a command straight into
	// metadata would route around the gate.
	"command":    true,
	"input_text": true,
	// The signal free-text keys signalDetailKeyFor writes. The client sets them
	// from Content (already gated), so these entries only matter if an adapter
	// ever sets them directly — which is exactly what this list is for.
	"denial_reason": true,
	"error_details": true,
}

// signalDetailKeyFor names the metadata key a signal's gated free text lands in.
// Per event type rather than one generic key, so the detail sits beside the
// structural fields a reader already has for that signal: `error_details` next
// to `error_type` on an APIError, `denial_reason` next to the tool identity on a
// PermissionDenied.
//
// Returns "" for every other event type, which drops the field: a signal detail
// on a tool result or a lifecycle event has no defined meaning, and inventing a
// key for it would put free text somewhere no reader expects one.
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
	// A signal's free text, when capture left it on the event. Capped like every
	// other gated body — the bytes this map produces are signed.
	if ev.Content != nil && ev.Content.SignalDetail != "" {
		if k := signalDetailKeyFor(ev.EventType); k != "" {
			m[k] = capBody(ev.Content.SignalDetail)
		}
	}
	m["event_id"] = ev.EventID
	// tool_name is carried here as well as in activity_type/activity_input
	// because metadata is the one blob every event type keeps: a consumer
	// grouping a session's events by tool does not have to know which of the
	// three shapes a given row came from.
	if ev.Tool.Name != "" {
		m["tool_name"] = ev.Tool.Name
	}
	if ev.Tokens != nil {
		m["tokens"] = ev.Tokens
	}
	if ev.Cost != nil {
		m["cost"] = ev.Cost
	}
	// The model that spent the tokens, carried here as well as in the turn's
	// activity_output for the same reason tool_name is: metadata is the one blob
	// every event type keeps, so a consumer grouping a session's spend by model
	// does not have to know which wire shape a row came from. Identifier-class,
	// bounded at the adapter (INV-2). SessionStarted already sets its own
	// metadata.model from the hook payload, so an adapter-supplied key wins —
	// the two agree by construction, since both are the provider's model id.
	if ev.Model != "" {
		if _, exists := m["model"]; !exists {
			m["model"] = ev.Model
		}
	}
	// The subagent a turn belongs to, so per-agent spend is attributable without
	// parsing the activity_id. Tool events already carry it via the adapter's
	// toolMetadata; this covers the turn events, whose metadata the mapper builds
	// from the hook payload's agent fields.
	if ev.AgentID != "" {
		if _, exists := m["agent_id"]; !exists {
			m["agent_id"] = ev.AgentID
		}
	}
	return json.Marshal(m)
}

// structuralActivityInput builds the INV-2-safe `activity_input` for an
// ActivityStarted — the identifiers the openbox-fe Verify-tab "Input" detail
// renders (log.input), and what core runs Guardrails stage "0" over
// (services/guardrail.go:180). It carries only structural fields: the tool
// name/kind and the file/mcp locators. It never carries content — the shell
// command, file text, or tool arguments — except on a gated call's evaluation
// event, which attaches the content below. Returns nil (field omitted) when
// nothing structural is known.
//
// That exception used to be one class of call (the approval escalation, shell
// and MCP only). ADR-0017 evaluates every gated class inline, so it now covers
// every gated call — including file writes, whose content is the file body. The
// gate is `content_capture`, unchanged: stripContent nils Content before this
// runs when the org has it off, and the observe copy of the same call never
// carries content on any path.
// contentKeyFor names the activity_input field a gated call's content lands in.
// "command" is kept for shell so the field an approver and every existing
// dashboard already read does not move.
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
	// The evaluation context, when the gated call carried one
	// (Content.ToolInput). It rides activity_input because that is the field
	// core's Guardrails stage 0, the approvals queue and the dashboard already
	// read, so content-aware policy and an approver both see it with no
	// server-side change. Content-gated: stripContent has already nil'd Content
	// when the org has content capture off, so it is simply absent then.
	//
	// The key names what the content IS, per class, because a reviewer reading
	// `command: <a file body>` would be misled about what was executed.
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
// ActivityCompleted — what core stores as the row's `output` and runs Guardrails
// stage "1" over (services/guardrail.go:192).
//
// It carries what the tool DID — byte and line counts, and the exit code when an
// adapter reports one — plus, under the content gate, what it PRODUCED.
//
// The result body used to be absent unconditionally, because tool output had no
// field to land in (SL3-SEC-3). ADR-0019 P1 retires that: the body is carried on
// Content.ToolOutput, which stripContent has already nil'd when the org has
// content capture off, so "absent" is now a posture rather than a structural
// property. It is capped here — the bytes this function feeds are the bytes
// buildPayload signs.
//
// The counts are the same Span fields the retired hook span carried at its root,
// re-homed rather than dropped. exit_code is read from metadata because the
// normalized event contract (dev-event.schema.json v1.0, frozen) has no field
// for it; no adapter supplies one today, so in practice it is absent, and a
// future adapter that sets metadata.exit_code gets it promoted with no client
// change.
//
// Returns nil (field omitted) when nothing is known — which is the honest state
// for a shell call, whose counts the providers do not expose.
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
	// What the tool produced, when capture left it on the event. Capped exactly
	// like the gated call's activity_input content: a single `cat` of a large
	// file would otherwise put megabytes on the wire per tool call.
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
//
// The client computes it because nothing else can any more: core used to derive
// the row's duration from the stored span, and there is no span. Core copies
// this field straight onto the row (storage_event.go:292-294) and the dashboard
// reads event.duration_ms directly, so an absent or wrong value is visible
// immediately rather than silently.
//
// The start time arrives cross-process: a PostToolUse hook has no idea when its
// PreToolUse fired, so the adapters' duration stash recovers ev.StartedAt and
// stamps it before the event is spooled (adapters/common/hookflow/duration.go).
//
// Returns nil — the field is OMITTED, not zero — when the stash missed, when the
// timestamps do not parse, or when the arithmetic is not positive. Zero would
// claim the call took no time, and a negative duration is nonsense; "unknown" is
// the true statement in all three cases, and omitting says it.
func durationMs(ev DevEvent) *float64 {
	start := rfc3339Nanos(firstNonEmpty(ev.StartedAt, ev.Timestamp))
	end := rfc3339Nanos(firstNonEmpty(ev.EndedAt, ev.Timestamp))
	if start == 0 || end <= start {
		return nil
	}
	ms := float64(end-start) / float64(time.Millisecond)
	return &ms
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
	ev.contentStripped = true
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
