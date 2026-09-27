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
	// RunGeneration is 0 for the original run; present only when non-zero, so
	// a generation-0 payload is byte-identical to a pre-1.8 one. Copied
	// straight from ev.RunGeneration -- the adapter is the one that stamps it
	// from the run record at hook time (see DevEvent.RunGeneration).
	RunGeneration int `json:"run_generation,omitempty"`
	// ContinuedFromRunID is the sealed run this one continues from, set only
	// on the WorkflowStarted of generation >= 1. Copied straight from
	// ev.ContinuedFromRunID (see DevEvent.ContinuedFromRunID).
	ContinuedFromRunID string `json:"continued_from_run_id,omitempty"`
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

	// cut accumulates every capBodyInto cut across every builder below, so
	// buildMetadata -- called last -- can serialize one combined summary.
	// One per event: see cutLog's doc comment.
	cut := &cutLog{}

	p := governanceEventPayload{
		Source:             source,
		EventType:          wireType,
		ActivityType:       activityLabel(ev), // additive dashboard label (pass-through column)
		WorkflowID:         workflowIDFor(ev),
		RunID:              runIDFor(ev),
		RunGeneration:      ev.RunGeneration,
		ContinuedFromRunID: ev.ContinuedFromRunID,
		WorkflowType:       workflowType,
		SignalName:         signalName, // "" (omitted) unless this is a SignalReceived
		Timestamp:          ev.Timestamp,
	}
	if signalName != "" {
		p.SignalArgs = buildSignalArgs(ev, cut) // nil (omitted) when there is nothing to show
	}

	switch ev.EventType {
	case EventToolCall:
		p.ActivityID = activityIDFor(ev)
		p.ActivityInput = structuralActivityInput(ev, cut)
	case EventToolResult:
		p.ActivityID = activityIDFor(ev)
		p.ActivityOutput = structuralActivityOutput(ev, cut)
		p.DurationMs = durationMs(ev)
		p.Status = statusFor(ev)
	case EventTurnStarted:
		p.ActivityID = turnActivityIDFor(ev)
		p.ActivityInput = turnActivityInput(ev)
	case EventTurnCompleted:
		p.ActivityID = turnActivityIDFor(ev)
		p.ActivityOutput = turnActivityOutput(ev, cut)
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

	meta, err := buildMetadata(ev, cut)
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

	// v1.8 observe-only lifecycle signals (21 classes): each rides stock
	// SignalReceived with signal_name = its EventType's snake_case, no
	// exceptions. Grouped under one comment so the diff reads as one decision.
	case EventSetup:
		return wireSignalReceived, "setup", nil
	case EventInstructionsLoaded:
		return wireSignalReceived, "instructions_loaded", nil
	case EventUserPromptExpansion:
		return wireSignalReceived, "user_prompt_expansion", nil
	case EventMessageDisplay:
		return wireSignalReceived, "message_display", nil
	case EventPermissionRequest:
		return wireSignalReceived, "permission_request", nil
	case EventPostToolBatch:
		return wireSignalReceived, "post_tool_batch", nil
	case EventNotification:
		return wireSignalReceived, "notification", nil
	case EventTaskCreated:
		return wireSignalReceived, "task_created", nil
	case EventTaskCompleted:
		return wireSignalReceived, "task_completed", nil
	case EventTeammateIdle:
		return wireSignalReceived, "teammate_idle", nil
	case EventConfigChange:
		return wireSignalReceived, "config_change", nil
	case EventCwdChanged:
		return wireSignalReceived, "cwd_changed", nil
	case EventDirectoryAdded:
		return wireSignalReceived, "directory_added", nil
	case EventFileChanged:
		return wireSignalReceived, "file_changed", nil
	case EventWorktreeRemove:
		return wireSignalReceived, "worktree_remove", nil
	case EventPreCompact:
		return wireSignalReceived, "pre_compact", nil
	case EventPostCompact:
		return wireSignalReceived, "post_compact", nil
	case EventPreModelSwitch:
		return wireSignalReceived, "pre_model_switch", nil
	case EventPostModelSwitch:
		return wireSignalReceived, "post_model_switch", nil
	case EventElicitation:
		return wireSignalReceived, "elicitation", nil
	case EventElicitationResult:
		return wireSignalReceived, "elicitation_result", nil

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

// runIDFor selects the wire run_id: it never computes one. ev.RunID when a
// continue-as-new minted it (the adapter stamped it from the run record at
// hook time), else ev.SessionID (generation 0, where the session id IS the
// run id -- this is why a generation-0 payload is byte-identical to a
// pre-1.8 one, see TestGoldenWirePayloads). Exactly two call sites read it --
// buildPayload (above) and ApprovalKeyFor (approval.go) -- and they must
// agree or an escalation and its poll disagree about which record to hit
// (TestApprovalKeyFor_MatchesTheWirePayload). Nowhere else: activityPairKey,
// turnActivityIDFor and workflowIDFor stay keyed on ev.SessionID, because
// activity ids are session-scoped by construction and a run id there would
// change every shipped idempotency key for zero benefit.
func runIDFor(ev DevEvent) string {
	if ev.RunID != "" {
		return ev.RunID
	}
	return ev.SessionID
}

// WireActivityID is the activity_id buildPayload puts on ev's wire payload,
// "" for an event type that carries none. It exists for the local trace,
// whose records must join the core rows they describe; the payload switch
// below and this one are pinned together by TestWireActivityIDMatchesThePayload.
func WireActivityID(ev DevEvent) string {
	switch ev.EventType {
	case EventToolCall, EventToolResult:
		return activityIDFor(ev)
	case EventTurnStarted, EventTurnCompleted:
		return turnActivityIDFor(ev)
	}
	return ""
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
func turnActivityOutput(ev DevEvent, cut *cutLog) json.RawMessage {
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
		m["thinking"] = capBodyInto(cut, "activity_output", "thinking", ev.Content.Thinking)
	}
	// What the model said; the two sources are mutually exclusive.
	switch {
	case ev.Span != nil && ev.Span.ResponseBody != "":
		// As the gateway stored it: an Anthropic-format stream arrives already
		// reassembled into one message document (gateway/eventstream.go), any
		// other body verbatim. Never reply_text -- see modelCallReplyKey.
		body := ev.Span.ResponseBody
		// capModelCallBody's own cut used to bypass cutLog entirely, so
		// truncated_paths stayed silent about the single largest thing a
		// model-call row cut. Exact byte compare, deliberately: capModelCallBody
		// itself compares bytes (see its doc comment), so a rune-based check
		// here would fire on a different body than the one actually cut.
		clientCut := len(body) > maxModelCallBodyBytes
		// truncated_paths is a COMPLETE index of what is incomplete on egress (owner
		// ruling), and the gateway's cut is exactly that: a body this
		// row admits is short. Keyed off the same bool captureNote ORs below, so the
		// note and the index cannot disagree. A gateway cut whose buffer lands at or
		// under our own cap leaves clientCut false, which is how one live row carried
		// truncated:true with the index silent.
		if clientCut || ev.Span.ResponseTruncated {
			cut.note("activity_output", modelCallContentKey)
		}
		m[modelCallContentKey] = capModelCallBody(body)
		// Inside this arm and nowhere else: this is the note's ONLY attachment
		// point (see captureNote's doc comment). It disappears under
		// content_capture:false along with ResponseBody itself, which is
		// correct -- there is no stored body left for it to describe.
		//
		// len(body), not ev.Span.ResponseBytesSeen: that counter is the
		// gateway sink's own, incremented as bytes arrive OFF THE WIRE --
		// still compressed, since DisableCompression means the gateway decodes
		// capture itself rather than letting the transport do it silently
		// (gateway/proxy.go). gatewayemit's span() deliberately never copies
		// response headers onto the wire Span (see its own doc comment), so
		// this layer cannot ask "was this compressed" and must not guess -- and
		// a corpus measurement puts brotli+gzip at ~98% of real replies
		// (internal/gateway/decode.go), so treating that raw wire count as if
		// it were decoded-content bytes made original_bytes routinely SMALLER
		// than the content it was supposed to describe: backwards for a field
		// a UI reads as "how much of the reply is this".
		//
		// body is already decoded here -- capturableBody/decodeCapturable ran
		// before this ever reached the wire -- or is a documented marker
		// string when decoding itself failed, so its own length is always the
		// same post-decode unit as content: exact for a complete reply, and a
		// true (if not maximally tight) lower bound for one the gateway or
		// this client's own capModelCallBody cut. That includes the marker
		// case: the true original size is genuinely unknowable there, and the
		// marker's own length is the honest answer rather than a fabricated
		// one.
		m[captureSummaryKey] = captureNote{
			Truncated:     ev.Span.ResponseTruncated || clientCut,
			OriginalBytes: len(body),
		}
	case ev.Content != nil && ev.Content.Output != "":
		// Same bypass, same fix, on the hook lane's two keys: both are cut from
		// the same source string, so a cut records both paths.
		if len(ev.Content.Output) > maxModelCallBodyBytes {
			cut.note("activity_output", modelCallContentKey)
			cut.note("activity_output", modelCallReplyKey)
		}
		m[modelCallContentKey] = capModelCallBody(ev.Content.Output)
		// Inside this arm, never after the switch. See modelCallReplyKey: the
		// arm IS the lane discriminator, and a condition placed after the
		// switch is one a later refactor can drop without failing anything
		// visible.
		m[modelCallReplyKey] = capModelCallBody(ev.Content.Output)
	}
	return marshalOrNil(m)
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

	// v1.8: the 7 new per-class keys signalDetailKeyFor can return.
	"requested_tool_input": true,
	"notification_message": true,
	"task_subject":         true,
	"compact_instructions": true,
	"compact_summary":      true,
	"elicitation_message":  true,
	"elicitation_response": true,

	// v1.9: metadata-native content, with no signalDetailKeyFor entry. Their
	// classes already spend Content.SignalDetail on the sibling key above
	// (notification_message, task_subject), and that carrier holds one string —
	// so these ride metadata, and this list is their only gate.
	"notification_title": true, // Notification.title
	"task_description":   true, // Task{Created,Completed}.task_description
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
	// v1.8: 8 cases, 7 distinct keys — the count is of the v1.8 block below, not
	// of the whole switch, which is 10 cases and 9 distinct keys with the two
	// above. Counted twice already; leave the scoping explicit. No case for
	// EventUserPromptExpansion — it is structural-only (D1).
	case EventPermissionRequest:
		return "requested_tool_input"
	case EventNotification:
		return "notification_message"
	case EventTaskCreated:
		return "task_subject"
	case EventTaskCompleted:
		return "task_subject"
	case EventPreCompact:
		return "compact_instructions"
	case EventPostCompact:
		return "compact_summary"
	case EventElicitation:
		return "elicitation_message"
	case EventElicitationResult:
		return "elicitation_response"
	}
	return ""
}

// eventMetadataForEgress copies the adapter's metadata map, applying the two
// rules that hold wherever those keys egress: INV-2's content gate and the
// http_status row rule.
//
// buildMetadata and buildSignalArgs both call it, and that is the point. The
// same map now reaches two wire fields, so a gate implemented twice is a gate
// that drifts: the copy that forgets the skip lets an adapter writing a content
// key into metadata route straight around content_capture. One implementation
// makes contentMetadataKeys' completeness rule checkable rather than
// aspirational — see TestContentBearingMetadataIsGatedInSignalArgs.
func eventMetadataForEgress(ev DevEvent, cut *cutLog, dest string) map[string]any {
	m := make(map[string]any, len(ev.Metadata)+4)
	for k, v := range ev.Metadata {
		if contentMetadataKeys[k] {
			if ev.contentStripped {
				continue // INV-2: gated content never rides the metadata blob either
			}
			// Content gets the content bound wherever it rides. Text on
			// Content.* is capped by capBody at the point it is attached; the
			// same text arriving through metadata had none, so an unbounded
			// free-text key was an unbounded body on the wire. Bounds have
			// owners, and capBody owns content egress -- not one carrier of it.
			if s, ok := v.(string); ok {
				v = capBodyInto(cut, dest, k, s)
			}
		}
		if k == "http_status" && !observesAResponse(ev.EventType) {
			continue // see observesAResponse; the key is barred by the ROW's meaning
		}
		if k == captureSummaryKey {
			// Client-reserved: this key is SYNTHESIZED below from cutLog, never
			// carried. Barring it here closes two holes at once. First, an
			// adapter-written value would ride signal_args, which OPA and
			// Guardrails read -- unlike metadata, which no governance engine
			// evaluates. Second, buildMetadata only overwrites the key when
			// something was actually cut, so on an uncut event a caller-supplied
			// value would pass through as a FORGED truncation summary. Barred at
			// the one door both destinations come through, so the summary's
			// correctness stops depending on builder call order.
			continue
		}
		m[k] = v
	}
	return m
}

func buildMetadata(ev DevEvent, cut *cutLog) (json.RawMessage, error) {
	m := eventMetadataForEgress(ev, cut, "metadata")
	addSignalDetail(m, ev, cut, "metadata")
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
		setIfAbsent(m, "model", ev.Model)
	}
	if ev.AgentID != "" {
		setIfAbsent(m, "agent_id", ev.AgentID)
	}
	// Rehomed from span attributes, which never persisted. NOT in
	// contentMetadataKeys: derived evidence, not content.
	if s := ev.Span; s != nil {
		if s.CredentialFingerprint != "" {
			setIfAbsent(m, "credential_fingerprint", s.CredentialFingerprint)
		}
		if s.HTTPStatus != 0 && observesAResponse(ev.EventType) {
			setIfAbsent(m, "http_status", s.HTTPStatus)
		}
		// Here and not only in activity_input, which the content gate empties: the
		// method and URL are account-binding evidence, and docs/data-and-privacy.md
		// says they ship under content_capture:false. They used to ride a span
		// field stripContent never cleared; once that went, capture-off stored a
		// relayed call with no method or URL anywhere. Duplicated with
		// activity_input rather than moved, where being unlisted makes the
		// alignment judge's cap drop the pair before `content`.
		if s.HTTPMethod != "" {
			setIfAbsent(m, "http_method", s.HTTPMethod)
		}
		if s.HTTPURL != "" {
			setIfAbsent(m, "http_url", s.HTTPURL)
		}
		// Claude Code's own per-call correlation block (v1.9), parsed from the
		// raw request body. Same treatment as CredentialFingerprint above: NOT
		// in contentMetadataKeys, ungated, derived evidence rather than
		// content. IsSubagent is the one boolean, and it is promoted only when
		// true -- its zero value means "the source entry was absent", and a
		// stored `false` would read as a confirmed non-subagent, which this
		// lane never has grounds to claim.
		if s.PromptID != "" {
			setIfAbsent(m, "prompt_id", s.PromptID)
		}
		if s.PreviousRequestID != "" {
			setIfAbsent(m, "previous_request_id", s.PreviousRequestID)
		}
		if s.IsSubagent {
			setIfAbsent(m, "is_subagent", true)
		}
		if s.Entrypoint != "" {
			setIfAbsent(m, "entrypoint", s.Entrypoint)
		}
	}
	// One combined summary of every capBodyInto cut across this event's wire
	// objects (metadata + signal_args), keyed by destination-qualified path so
	// the same value cut into both is two entries, not one (see cutLog).
	// Placed last and omitted when nil: absence means nothing was truncated.
	// NOT in contentMetadataKeys -- derived evidence, not content -- and safe
	// from the :506 backstop above, which only ever sees ev.Metadata's own
	// keys and has already returned by the time this line runs.
	if paths := cut.sorted(); paths != nil {
		m[captureSummaryKey] = map[string]any{"truncated_paths": paths}
	}
	return json.Marshal(m)
}

// contentKeyFor names the activity_input key a tool's body lands under. It
// takes the span semantic as well as the kind because an llm_tool_call is
// shell-kinded but must NEVER land under `command`: `command` is an
// enforcementKey that 71 of the control pack's 104 conditions match, and a
// model-written prompt arriving there would be judged by the 17 shell
// templates. See enforcementkeys_test.go.
func contentKeyFor(kind ToolKind, sem string) string {
	if kind == ToolShell && sem == "llm_tool_call" {
		return "arguments"
	}
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

// structuralActivityInput builds the INV-2-safe `activity_input` for an
// ActivityStarted: the identifiers the Verify tab's "Input" detail renders, and
// what the control plane runs its first Guardrails stage over.
func structuralActivityInput(ev DevEvent, cut *cutLog) json.RawMessage {
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
		sem := ""
		if ev.Span != nil {
			sem = ev.Span.SemanticType
		}
		key := contentKeyFor(ev.Tool.Kind, sem)
		m[key] = capBodyInto(cut, "activity_input", key, ev.Content.ToolInput)
	}
	return marshalOrNil(m)
}

// structuralActivityOutput returns nil (field omitted) when nothing is known;
// which is the honest state for a shell call, whose counts the providers do
// not expose.
func structuralActivityOutput(ev DevEvent, cut *cutLog) json.RawMessage {
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
		m["output"] = capBodyInto(cut, "activity_output", "output", ev.Content.ToolOutput)
	}
	return marshalOrNil(m)
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

// buildSignalArgs fills the one field a governance engine reads on a
// SignalReceived. OPA matches signal_name + signal_args; Guardrails read
// signal_args and nothing else. metadata has no reader in either, so a signal
// class whose payload lives only there is write-only telemetry: the engine can
// match THAT a config_change happened, never that it touched `.env`.
//
// Two arms, and the split is the whole safety argument:
//
//   - prompt_submitted's signal_args IS the goal. Core's stringifySignalArgs
//     turns it into the text every later action is scored against. Its shape is
//     a shipped contract; the arm stays verbatim and takes nothing from the
//     projection.
//   - Every other signal projects. Uniform, with no per-class knowledge, because
//     a switch drifts on the first class someone adds to the mapper and forgets
//     here — which is exactly how 21 classes shipped invisible. See
//     TestSignalArgsProjectionCoversEveryClass.
//
// Projected, never moved: metadata keeps every key, because docs/mapping.md's
// correlation keys and the SQL forensics path read metadata. The cost is a few
// hundred duplicated bytes on ~140 rows a session, and it is accepted.
//
// ORDERING HAZARD, recorded at the site that causes it. A core without the
// source-and-name goal gate reads ANY non-empty signal_args as a new user goal.
// Be precise about what that does and does not depend on: stringifySignalArgs
// prefers ["prompt","message","input","text","content"] and then falls back to
// the RAW JSON of the whole object, so on an ungated core every projected signal
// overwrites the goal whatever its keys are called. The five names only decide
// whether the resulting goal reads as prose or as `{"batch_size":1,...}`.
//
// They are still worth pinning, because a prose-shaped wrong goal is the harder
// one to notice — TestSignalArgsProjectionDoesNotUseCoreGoalKeys and its two
// adapter-side twins do that. But the GATE is what makes this safe, not the
// naming: a build carrying this projection must not reach a developer before
// that gate is running in prod.
func buildSignalArgs(ev DevEvent, cut *cutLog) json.RawMessage {
	var m map[string]any
	if ev.EventType == EventPromptSubmitted {
		m = map[string]any{}
		if ev.Content != nil && ev.Content.Prompt != "" {
			m["prompt"] = capBodyInto(cut, "signal_args", "prompt", ev.Content.Prompt)
		}
	} else {
		// The same gate buildMetadata applies, from the same function, so a
		// content key cannot be gated in one destination and not the other.
		m = eventMetadataForEgress(ev, cut, "signal_args")
		addSignalDetail(m, ev, cut, "signal_args")
	}
	return marshalOrNil(m)
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

// cutLog accumulates the wire paths capBody shortened while one payload is
// built. Not safe for concurrent use and does not need to be: buildPayload
// owns exactly one per event.
type cutLog struct{ paths []string }

// note records a cut at dest+"."+key. Called only when the cap actually
// fired. Nil-safe: capBodyInto's nil *cutLog callers (capBody, and any test
// that does not care about the summary) need no branch of their own.
func (c *cutLog) note(dest, key string) {
	if c == nil {
		return
	}
	c.paths = append(c.paths, dest+"."+key)
}

// sorted returns the paths lexicographically, or nil when nothing was cut, so
// the caller can omit the key.
func (c *cutLog) sorted() []string {
	if c == nil || len(c.paths) == 0 {
		return nil
	}
	out := slices.Clone(c.paths)
	slices.Sort(out)
	return out
}

// capBodyInto caps s, recording a cut at dest+"."+key when the cap fired.
// One writer, so the dynamic backstop (eventMetadataForEgress) cannot drift
// from the static sites.
func capBodyInto(c *cutLog, dest, key, s string) string {
	if len(s) <= maxBodySize { // fast path: byte len ≤ cap ⇒ rune count ≤ cap
		return s
	}
	r := []rune(s)
	if len(r) <= maxBodySize {
		return s
	}
	c.note(dest, key)
	return string(r[:maxBodySize])
}

// capBody is capBodyInto with no accumulator: a cut still happens, nothing
// records it. Kept so direct callers (and TestCapBodyStillCutsRunesForEvery
// OtherField's CJK control, which never triggers a cut anyway) stay untouched.
func capBody(s string) string {
	return capBodyInto(nil, "", "", s)
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

// modelCallReplyKey is what makes an assistant turn judgeable at all.
//
// Core's replyTextFromActivityOutput (openbox-core
// internal/services/goal_alignment.go) reads this exact top-level string off
// activity_output, and its model-turn judge keys the whole branch on the key's
// PRESENCE with deliberately NO fallback to `content`: old producers keep
// sending SSE frames there indefinitely, and a fallback would feed the judge
// exactly what the branch exists to remove. So the key must be distinct, and it
// must be absent rather than empty on a lane that has only SSE.
//
// Symbols, not line numbers, on purpose: the core side moves independently of
// this repo, and a citation pointing at the wrong line is worse than none.
//
// Which is why it is set inside turnActivityOutput's Content.Output arm alone.
// That arm is the hook lane — Content.Output for a turn is set at exactly one
// site — internal/adapters/claude-code/mapper.go's MapTurn, from the Stop
// hook's LastAssistantMessage — and the presence test means every row carrying this
// key costs a judge call. A live session ran 137 span-sourced model calls
// against 2 hook-sourced turns; sourcing this from Span.ResponseBody as well
// would multiply the cost by roughly 70 on a judge fleet doing ~7 judgements a
// minute.
//
// `content` stays alongside it. OPA reads activity_output ungated by event
// type, so `content` is the surface policy sees today; removing it would be a
// non-additive wire change. The duplicated bytes are the price, on 2 rows a
// session, both capped by capModelCallBody.
const modelCallReplyKey = "reply_text"

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
//
// captureSummaryKey names TWO sibling objects that share the one key name at
// two different destinations, and never collide because the destinations
// never do:
//   - `metadata.openbox_capture` is buildMetadata's cutLog summary,
//     {truncated_paths}, client-reserved and barred from inbound metadata by
//     eventMetadataForEgress so it can only ever be synthesized from cutLog
//     (see truncationsummary_test.go).
//   - `activity_output.openbox_capture` is turnActivityOutput's captureNote,
//     {truncated, original_bytes} -- the response-side twin of gateway's
//     selectionNote (openbox_selection). No bar applies here: it is assembled
//     directly from ev.Span and a local cut check, never copied from caller
//     metadata, so there is nothing for eventMetadataForEgress to guard.
const captureSummaryKey = "openbox_capture"

// captureNote is activity_output's response-completeness note: whether the
// model-call reply the row stores is the whole thing. Truncated has no
// omitempty -- false IS the wire value that says "this reply is complete,"
// and dropping it on the zero case would silently turn "unknown" and
// "complete" into the same absence, the exact defect this note exists to
// remove. OriginalBytes has none either, matching selectionNote's own field.
type captureNote struct {
	Truncated     bool `json:"truncated"`
	OriginalBytes int  `json:"original_bytes"`
}

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
	return marshalOrNil(m)
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

// marshalOrNil renders one wire object, or nil -- the field omitted -- when
// there is nothing to say or the object cannot be encoded. Five builders close
// this way, so the omit-on-error convention has one implementation.
func marshalOrNil(m map[string]any) json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// setIfAbsent writes v under k only when the adapter's own metadata did not
// already carry k: a caller-supplied value wins over a derived one.
func setIfAbsent(m map[string]any, k string, v any) {
	if _, exists := m[k]; !exists {
		m[k] = v
	}
}

// addSignalDetail attaches this class's free-text detail under its own key,
// capped and recorded against dest. Both destinations of
// eventMetadataForEgress carry it, so it is written once for the same reason
// that gate is.
func addSignalDetail(m map[string]any, ev DevEvent, cut *cutLog, dest string) {
	if ev.Content == nil || ev.Content.SignalDetail == "" {
		return
	}
	if k := signalDetailKeyFor(ev.EventType); k != "" {
		m[k] = capBodyInto(cut, dest, k, ev.Content.SignalDetail)
	}
}
