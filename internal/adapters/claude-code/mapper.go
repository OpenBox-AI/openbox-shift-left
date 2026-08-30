package claudecode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// provider is the constant provider tag carried in metadata (MAPPING.md §2).
const provider = "claude-code"

// agentToolName is the tool.name used for session-lifecycle events, which
// are produced by the coding agent itself rather than a discrete tool —
// matching the conformance testdata convention ({name:"claude-code",
// kind:"shell"}).
const agentToolName = provider

// Identity is the developer-agent identity the adapter emits under. It is
// minted by `openbox init` and read from the OS secret store by the
// hook binary (creds.go). Only the DID is needed to build events; the
// obx_ key and Ed25519 seed live in the client, never here (INV-1).
type Identity struct {
	DeveloperDID string // did:aip:<uuid>
}

// Mapper translates Claude Code hook payloads into normalized DevEvents.
// It is a pure function of (hook, payload, identity, clock, id source) — no
// I/O — so the whole acceptance surface (INV-2 metadata-only, tool
// classification, semantic-type hints) is unit-testable.
type Mapper struct {
	Identity Identity
	Now      func() time.Time // injectable clock; defaults to time.Now
	// NewID, when non-nil, overrides the idempotency-id source (INV-5) —
	// used by tests to pin ids. When nil (the production default), the id
	// is derived deterministically from the event's own structural fields
	// (deriveID): the same event always yields the same id (robust if ever
	// recomputed from the spooled record) and two distinct events never
	// collide.
	NewID func() string
	// Finops, when non-nil, carries the usage numbers only the finops
	// reader extracted from the SessionEnd transcript. Map copies them
	// onto the SessionEnded event only. nil (the default) ⇒ events carry
	// no tokens/cost. The Mapper itself does no file I/O: the
	// content-bearing transcript read + fail-open logging happen in
	// RunHook (which owns the logger), so this stays a pure mapping of its
	// inputs (like the injected Now / NewID), preserving the INV-2
	// guarantee that Map never touches content.
	Finops *FinopsUsage
	// CaptureContent authorizes copying the (content) prompt text onto the
	// emitted PromptSubmitted event. Default false = metadata-only (INV-2): the
	// prompt is never egressed. Set from ResolveContentCapture() in RunHook,
	// the same opt-in the client's Emit uses to decide whether to strip content
	// — so capture and egress always agree. Redaction at source is a separate
	// layer ([EXT-guardrail-redaction], inert locally); the prompt is capped
	// before egress (capBody, buildSignalArgs).
	//
	// It gates every content field, not just the prompt: the assistant's turn
	// text and, since that decision, tool input on the observe path, tool
	// output, and the lifecycle signals' free text. One flag for all of them is
	// deliberate — a second posture key would let an org believe it had opted
	// out of content while one class kept egressing.
	CaptureContent bool
	// RedactContent redacts a content body for secrets before it is attached to
	// an event. nil ⇒ identity, which is the honest `secret_detection:false`
	// case: the text egresses unredacted (that decision says so rather than
	// hiding it).
	//
	// It is a COLLABORATOR rather than something MapTurn remembers to call,
	// which is the point: with the redactor on the Mapper, every path that
	// attaches text goes through it by construction. A function the mapper had
	// to remember to invoke would be one refactor away from a path that forgot,
	// and the failure would be a secret at the control plane — the hardest
	// place to purge anything from. Same idiom as Now / NewID / Finops /
	// Posture; wired in RunHook from ResolveSecretDetection().
	RedactContent func(string) string
	// Posture, when non-nil, is the session's effective posture (E8-S5),
	// attached to the SessionStarted event's metadata only. RunHook resolves
	// it (config reads, a bundle hash, the freshness check) and passes it in,
	// so Map stays I/O-free — the same split as Finops. nil ⇒ no posture key,
	// which is what the enforce/observe tests and conformance fixtures see.
	Posture *devconfig.Posture
	// Evidence, when non-nil, records how much of this session's telemetry is
	// known to be undelivered at session end (E8-S7). Attached to the
	// SessionEnded event's metadata only. RunHook counts the carry-over files
	// and passes the result in, keeping Map I/O-free.
	Evidence *EvidenceState
}

// EvidenceState is the completeness of a session's telemetry as the client can
// see it at session end.
//
// It describes the spool BEFORE this session's final flush: a non-zero
// Undelivered means an earlier flush failed and those events are waiting in
// carry-over files. It is not a claim that they are lost — a later flush
// re-sends them, and the server deduplicates — so "degraded" here means
// "incomplete as of now", and only exceeding the retry bound makes loss
// permanent (which the spool logs).
type EvidenceState struct {
	Undelivered int
}

// metadata renders the evidence state. The state string is always present so a
// reader can distinguish "complete" from "this client does not report it".
func (e EvidenceState) metadata() map[string]any {
	state := "complete"
	if e.Undelivered > 0 {
		state = "degraded"
	}
	m := map[string]any{"evidence_state": state}
	if e.Undelivered > 0 {
		m["evidence_undelivered"] = e.Undelivered
	}
	return m
}

// FinopsUsage is the numbers-only usage rollup the finops reader produces
// from a transcript. It carries only Tokens/Cost value structs — no
// content, by construction (see usage.go).
type FinopsUsage struct {
	Tokens *client.Tokens
	Cost   *client.Cost
}

// NewMapper returns a Mapper with production defaults. NewID is left nil so
// the event_id is derived deterministically from each event's structural
// fields (deriveID / INV-5); the clock defaults to time.Now.
func NewMapper(id Identity) Mapper {
	return Mapper{Identity: id, Now: time.Now}
}

// Map converts one hook payload into a normalized DevEvent. The bool reports
// whether an event should be emitted at all: it is false when the payload is
// unusable (no session id, or no valid developer DID) — in which case the caller
// drops it fail-open (INV-3), never blocking the tool call.
//
// Map copies content onto an event ONLY under CaptureContent, and only after
// RedactContent has run over it (INV-2). With capture off it carries structural
// metadata alone — the tool identity, file paths, and lifecycle enums — which is
// the same shape it always produced. What changed with that decision is that the
// with-capture-on shape now includes tool input on the observe path, tool
// output, and the signals' free text; the gate, not the absence of a field, is
// what keeps them off the wire.
func (m Mapper) Map(hook HookName, e *HookEvent) (client.DevEvent, bool) {
	if e == nil || e.SessionID == "" {
		return client.DevEvent{}, false
	}
	if !strings.HasPrefix(m.Identity.DeveloperDID, "did:aip:") {
		return client.DevEvent{}, false
	}

	now := m.clock()
	// RFC3339Nano (not RFC3339): the sub-second precision is the per-event
	// distinguisher deriveID folds into the id so two same-tool events in
	// the same wall-clock second never collide. It is byte-identical to
	// RFC3339 for a whole-second instant (Go omits an all-zero fraction)
	// and core parses it with RFC3339Nano (payload.go rfc3339Nanos), so
	// nothing downstream changes.
	ts := now.UTC().Format(time.RFC3339Nano)

	// WorkspaceID is left empty so the client uses the developer DID as
	// core's workflow_id. The DID is present on every event, so
	// (workflow_id, run_id) is stable per session regardless of which hook
	// fires — a cwd-derived id would fragment a session if any hook
	// omitted cwd. Per-workspace grouping is still available via
	// metadata.cwd on SessionStarted (MAPPING.md §1: DID is a blessed
	// workflow_id).
	ev := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		SessionID:     e.SessionID,
		DeveloperDID:  m.Identity.DeveloperDID,
		Timestamp:     ts,
	}

	switch hook {
	case HookSessionStart:
		ev.EventType = client.EventSessionStarted
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = mergeMetadata(
			sessionStartMetadata(e),
			// Account attribution, once per session (that decision req 6). Read
			// from the client's own local record, so a session is attributable
			// to an org even where the gateway is not running. Silent on any
			// failure: an optional attribution field must never stop a session
			// reporting.
			accountMetadata(localAccount(homeDir())))
		// Effective posture as evidence (E8-S5): structural booleans and
		// opaque ids only, so it is INV-1/INV-2 safe to egress.
		if m.Posture != nil {
			ev.Metadata["posture"] = m.Posture.Metadata()
		}

	case HookUserPromptSubmit:
		ev.EventType = client.EventPromptSubmitted
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		// Metadata-only by default (INV-2): permission_mode is session
		// context (shown in the dashboard Overview, not as the prompt's
		// Input). Token/cost are not exposed to Claude Code hooks, so they
		// are absent.
		ev.Metadata = mergeMetadata(
			compact(map[string]any{"permission_mode": enumOr(e.PermissionMode, permissionModes)}),
			subagentMetadata(e))
		// The prompt is the signal's input and it is content, so it is
		// carried on ev.Content.Prompt only under the content-capture
		// opt-in — where it becomes the SignalReceived signal_args
		// (buildSignalArgs, capped). Default off ⇒ Content stays nil and
		// the prompt never egresses (Emit would strip it anyway).
		//
		// REDACTED, like every other content field on this event. It was the one
		// that was not: `Prompt: e.Prompt` reached the wire unscanned while
		// Output, Thinking, ToolInput, ToolOutput and SignalDetail all pass
		// through m.redact — so a developer pasting a credential into a prompt
		// shipped it verbatim with secret_detection fully ON. Map's own doc
		// comment ("only after RedactContent has run over it") and README's
		// "scanned for secrets and redacted locally first" both describe the
		// behaviour this line now has, and described something else before.
		if m.CaptureContent && e.Prompt != "" {
			if p := m.redact(e.Prompt); p != "" {
				ev.Content = &client.Content{Prompt: p}
			}
		}

	case HookPreToolUse:
		ev.EventType = client.EventToolCall
		ev.StartedAt = ts
		ev.Tool, ev.Span = mapTool(e, "started")
		ev.Metadata = toolMetadata(e)
		// What the tool was asked to do, on the OBSERVE path — the shell
		// command, the MCP arguments, the file body. This is the change that
		// retires SL3-SEC-3 ("tool commands and file bodies never egress on
		// observe events"): the guarantee becomes a posture, not a structural
		// property, and the same gate that covers the prompt covers this.
		//
		// evaluationContext is reused rather than reimplemented so the observe
		// copy and the gated /evaluate copy of one call carry the IDENTICAL
		// extract. Two copies of one call that disagreed about the command
		// would be worse than either alone. The gated path overwrites this with
		// its own precisely-redacted rebuild (enforceTarget.DevEvent).
		if m.CaptureContent {
			// Redact and attach; the EGRESS cap is the client's capBody, like
			// every other content field.
			//
			// It used to be hookflow.CapCommand — MaxCommandLen, 8 KiB — whose own
			// doc says it bounds the LOCAL DecisionRequest command and is "never
			// egressed". Reusing a local-only bound as an egress bound made the
			// wire cap 8x smaller than every document describing it:
			// docs/data-and-privacy.md says a Write body is "truncated at 64KB"
			// and CLAUDE.md says content-based policy "sees at most the first 64KB
			// of any body". A 40 KB source file egressed as 8 KiB, and core's
			// Guardrails stage 0 and any approver saw only that. It also coupled
			// two unrelated numbers: changing MaxCommandLen for local-matching
			// reasons silently changed what the server can see.
			//
			// Still bounded: m.redact truncates at MaxRedactBody before scanning,
			// which is the same shape ToolOutput and Thinking already have. And
			// redact still runs BEFORE any cap, so a secret straddling a boundary
			// is replaced rather than cut into an unmatchable fragment.
			if in := m.redact(toolInputExtract(e, nil)); in != "" {
				ev.Content = &client.Content{ToolInput: in}
			}
		}

	case HookPostToolUse:
		ev.EventType = client.EventToolResult
		ev.EndedAt = ts
		ev.Tool, ev.Span = mapTool(e, "completed")
		ev.Metadata = toolMetadata(e)
		// Which hook fired IS the outcome — nothing is inferred and nothing is
		// read out of the tool's output. Claude Code splits the two: PostToolUse
		// is documented as "Run after successful tool" and PostToolUseFailure as
		// "Run after tool fails", and they are mutually exclusive per call
		// (verified empirically on 2.1.229 — a failing Bash fired
		// PostToolUseFailure and no PostToolUse).
		//
		// That exclusivity is what makes an unconditional "completed" truthful
		// here. If a future version fired both, this line would report SUCCESS
		// 100% on failing calls — a worse failure than the 0% it replaces,
		// because it is believable. The probe is the standing evidence, and the
		// pairing is re-checked whenever the hook surface changes.
		ev.Status = client.StatusCompleted
		// What the call PRODUCED. Gated, redacted, then capped by the client —
		// see gatedToolOutput.
		ev.Content = m.gatedToolOutput(e.toolOutputText())

	case HookPostToolUseFailure:
		// The failure half of PostToolUse, and the same event on the wire: a
		// call that failed is still a completed ACTIVITY — it started, it
		// finished, it took time. Only the outcome differs, which is exactly
		// what `status` is for. Routing it to its own event type instead would
		// leave the started half unpaired and the failure invisible to every
		// consumer that reads ActivityCompleted.
		ev.EventType = client.EventToolResult
		ev.EndedAt = ts
		ev.Tool, ev.Span = mapTool(e, "completed")
		ev.Metadata = toolMetadata(e)
		ev.Status = client.StatusFailed
		// A cancelled call is not a broken tool. Both are failures, but an
		// operator staring at a red Tool Health panel needs to tell them apart.
		// Absent stays absent (see HookEvent.IsInterrupt).
		if e.IsInterrupt != nil {
			ev.Metadata["is_interrupt"] = *e.IsInterrupt
		}
		// A failed activity's OUTPUT is its error text, so the tool's own
		// free-text `error` lands in the same gated field a successful call's
		// result body does — `status` already says which it is, and a second
		// field would split one question across two places.
		//
		// It is the SAME JSON key StopFailure uses for its closed provider enum
		// (HookEvent.ErrorType), which is why routing matters: this arm sends it
		// to gated CONTENT, the StopFailure arm sends it through enumOr. Free
		// text still has no path to metadata.error_type — pinned by
		// TestMap_FreeTextErrorNeverEgresses and conformance C37.
		ev.Content = m.gatedToolOutput(e.ErrorType)

	case HookSubagentStart:
		ev.EventType = client.EventSubagentStarted
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = subagentMetadata(e)

	case HookPermissionDenied:
		// That a denial happened, which tool it was about, and — under the
		// content gate — WHY. The tool identity and locators come from the same
		// mapTool the call itself used, so the denial correlates with the
		// PreToolUse that preceded it by tool_use_id.
		//
		// The reason is free text a classifier wrote, so it rides gated content
		// and reaches metadata.denial_reason. It must never reach signal_args —
		// see Content.SignalDetail for what core would do with it.
		ev.EventType = client.EventPermissionDenied
		ev.Tool, ev.Span = mapTool(e, "completed")
		ev.Metadata = toolMetadata(e)
		ev.Content = m.gatedSignalDetail(e.Reason)

	case HookStopFailure:
		// The model could not answer. error_type is the provider's own closed
		// enum, passed through enumOr — anything outside the ten known values is
		// dropped rather than egressed, which is also what keeps
		// PostToolUseFailure's free-text `error` (same JSON key) off the wire.
		ev.EventType = client.EventAPIError
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = mergeMetadata(
			compact(map[string]any{"error_type": enumOr(e.ErrorType, apiErrorTypes)}),
			subagentMetadata(e))
		// The provider's elaboration of that enum, gated: "rate_limit" says the
		// class, "retry after 60s" says what to do about it.
		ev.Content = m.gatedSignalDetail(e.ErrorDetails)

	case HookSessionEnd:
		ev.EventType = client.EventSessionEnded
		ev.EndedAt = ts
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = compact(map[string]any{"reason": enumOr(e.Reason, reasonValues)})
		// Attach the transcript usage rollup, if the finops reader
		// extracted any. Numbers only — Tokens/Cost carry no content
		// (usage.go). nil ⇒ nothing attached (finops off or empty session).
		if m.Finops != nil {
			ev.Tokens = m.Finops.Tokens
			ev.Cost = m.Finops.Cost
		}
		// Telemetry completeness as the client sees it (E8-S7).
		if m.Evidence != nil {
			ev.Metadata = mergeMetadata(ev.Metadata, m.Evidence.metadata())
		}

	case HookStop, HookSubagentStop:
		// Deliberately no single event. A turn is a PAIR, and both halves come
		// from MapTurn, which additionally needs the transcript window RunHook
		// read. Returning false here is the correct answer to "map this one hook
		// payload to one event" — not a gap. RunHook routes these hooks to
		// MapTurn instead of Observe.
		return client.DevEvent{}, false

	default:
		return client.DevEvent{}, false
	}

	// Derive the idempotency id LAST, from the now fully-populated structural
	// fields (INV-5). Done here rather than at struct-literal time so the
	// distinguishers set inside the switch (event_type, tool, span locator) all
	// feed the derivation.
	ev.EventID = m.eventID(ev)
	return ev, true
}

// MapTurn builds one model turn's ActivityStarted/ActivityCompleted pair from a
// Stop/SubagentStop firing and the transcript window it delimits.
//
// BOTH halves come from this one call, because both come from one hook firing.
// Stop fires at turn *end*, so the alternative was to open the pair from
// UserPromptSubmit and close it here — which would need two processes to agree on
// a turn index, would orphan a Completed when no prompt preceded the turn, and
// would open a turn per queued prompt. Deriving the pair atomically here avoids
// all three, and costs nothing observable: core takes the turn's duration from
// `duration_ms` on the Completed half and never reads the Started timestamp.
//
// The Started half's timestamp is the window's real open time when the transcript
// gave one, falling back to hook wall time. `duration_ms` is emitted only in the
// former case — a fabricated duration from a fallback start would be a made-up
// measurement, and the client omits the field rather than claiming zero.
//
// Returns ok=false when the payload is unusable or the window carried no usage: a
// Stop that opened no new tokens is not a turn, and emitting an empty pair would
// inflate the pair count Phase 06 asserts against the real turn count.
//
// Like Map, this is pure: RunHook does the transcript I/O and hands the result
// in, so the mapper still cannot touch content.
func (m Mapper) MapTurn(e *HookEvent, w turnWindow, index int) (started, completed client.DevEvent, ok bool) {
	if e == nil || e.SessionID == "" || !w.HasUsage {
		return client.DevEvent{}, client.DevEvent{}, false
	}
	if !strings.HasPrefix(m.Identity.DeveloperDID, "did:aip:") {
		return client.DevEvent{}, client.DevEvent{}, false
	}

	now := m.clock()
	closeTS := now.UTC().Format(time.RFC3339Nano)
	// The open timestamp is derived from a time.Time, never copied from the
	// transcript's string — so the raw transcript timestamp cannot reach the wire
	// even by accident (asserted in usage_test.go).
	openTS := closeTS
	haveRealOpen := false
	if !w.Open.IsZero() {
		openTS = w.Open.UTC().Format(time.RFC3339Nano)
		haveRealOpen = true
	}

	turnIndex := index
	base := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		SessionID:     e.SessionID,
		DeveloperDID:  m.Identity.DeveloperDID,
		Tool:          client.Tool{Name: agentToolName, Kind: client.ToolShell},
		TurnIndex:     &turnIndex,
		AgentID:       capStr(e.AgentID),
	}

	started = base
	started.EventType = client.EventTurnStarted
	started.Timestamp = openTS
	started.StartedAt = openTS
	started.Metadata = turnMetadata(e, turnIndex)
	started.EventID = m.eventID(started)

	completed = base
	completed.EventType = client.EventTurnCompleted
	completed.Timestamp = closeTS
	completed.EndedAt = closeTS
	// StartedAt drives durationMs in the client. Set it only when the open time
	// was really observed; otherwise leave it empty so the client omits the field
	// rather than reporting a duration of zero.
	if haveRealOpen {
		completed.StartedAt = openTS
	}
	completed.Tokens = w.tokens()
	// The one egressing string, bounded here at the untrusted boundary exactly as
	// metadata.model is (sessionStartMetadata).
	completed.Model = capStr(w.Model)
	// The assistant's answer, on the COMPLETED half only. Three conditions, all
	// required: the org opted into content capture, the hook actually carried
	// the field, and the redactor has run over it. The client then wraps it into
	// the one span core's alignment extractor reads, caps it at 64KB, and —
	// independently — drops it entirely if capture is off at flush time.
	//
	// Deliberately NOT on the started half: a turn's input is the prompt, which
	// already rides PromptSubmitted under the same gate. Duplicating it here
	// would double the egress for no reader.
	//
	// Thinking joins it on the same half and under the same gate (v1.4, that
	// decision amendment), from the transcript window rather than a hook field —
	// no hook carries thinking. Two fields on ONE Content, built together: two
	// separate assignments would have the second silently discard the first.
	if m.CaptureContent {
		var c client.Content
		if e.LastAssistantMessage != "" {
			c.Output = m.redact(e.LastAssistantMessage)
		}
		if w.Thinking != "" {
			c.Thinking = m.redact(w.Thinking)
		}
		if c.Output != "" || c.Thinking != "" {
			completed.Content = &c
		}
	}
	completed.Metadata = turnMetadata(e, turnIndex)
	completed.EventID = m.eventID(completed)

	return started, completed, true
}

// turnMetadata builds a turn event's metadata: the turn index plus the subagent
// correlation ids. Identifiers and one integer, never content (INV-2). The model
// is not put here — the client promotes it from DevEvent.Model, so it lands on
// metadata and in activity_output from one source rather than two.
func turnMetadata(e *HookEvent, index int) map[string]any {
	m := compact(map[string]any{
		"agent_id":   capStr(e.AgentID),
		"agent_type": capStr(e.AgentType),
	})
	m["turn_index"] = index
	return m
}

// mapTool builds the Tool identity and the semantic Span for a Pre/PostToolUse
// event. stage is "started" (ToolCall) or "completed" (ToolResult).
//
// Two identities, deliberately separate (client.Span.InvocationID /
// OperationID):
//
// - InvocationID = tool_use_id, which Claude Code mints per call. It keys the
// cross-process duration stash, so the completed hook recovers when the started
// one fired. - OperationID = what is being done, identical across a retry.
// activity_id derives from it, and activity_id is the approval key.
//
// These used to be the same value — tool_use_id was carried on span.function and
// fed both — so every retry became a different activity. An approver's decision
// could never be consumed: the retry filed a fresh approval request, and the
// rewake's "re-run to proceed" looped, burning one human decision per pass. Seen
// live: three attempts in one session, three approval ids, no output.
//
// The discriminator is per class, and matches what core's own
// ComputeApprovalFingerprint keys on, so the two agree about what "the same
// operation" means:
//
// - shell → the command, hashed. Approving `ls` must not grant `rm -rf /`. - MCP
// → the argument shape, hashed, beside the real function name. Core states the
// rule outright: "same tool with different arguments must require fresh
// approval". - anything else → the invocation, because those classes expose no
// structural discriminator to key on.
//
// That fallback used to be free: those classes were never escalated, so they
// could never hold an approval, and one event per call was the whole
// requirement. That decision gates every class, so they CAN hold one now, and an
// invocation-scoped key means an approval does not survive a retry — the
// developer re-runs the tool, the id moves, and a fresh request is filed rather
// than the granted one being found.
//
// That is a real limitation, and it is left standing deliberately: the fix is to
// give these classes a stable discriminator, which changes activity_id, which is
// this product's event identity — pinned byte-for-byte in
// client/approval_key_pin_test.go and load-bearing for core's dedupe and every
// stored row. Changing it is its own decision, not a side effect of widening the
// gate. The failure direction meanwhile is safe: an unmatched approval
// over-ASKS, and can never over-grant.
//
// Both fields are local: they are spooled so the enforce path and a later flush
// derive identical ids, and neither is emitted as a wire field — they only feed
// the already-opaque activity_id/span_id hashes. Structural or hashed
// throughout, never content (INV-2).
func mapTool(e *HookEvent, stage string) (client.Tool, *client.Span) {
	kind, sem, fileOp, mcpServer, function := classifyTool(e.ToolName)

	tool := client.Tool{Name: capStr(e.ToolName), Kind: kind}
	if kind == client.ToolMCP {
		tool.MCPServer = capStr(mcpServer)
	}

	span := &client.Span{SemanticType: sem, Stage: stage}
	switch {
	case isFileSemantic(sem):
		span.FilePath = capStr(e.filePath()) // structural locator only (INV-2)
		span.FileOp = fileOp
	case kind == client.ToolMCP:
		span.MCPServer = capStr(mcpServer)
		span.Function = capStr(function)
	}
	span.InvocationID = capStr(e.ToolUseID)
	span.OperationID = operationID(kind, e)
	return tool, span
}

// operationID derives the per-class operation discriminator described on
// mapTool. It reads the tool's own input, which stays local — only the hash
// reaches the id.
func operationID(kind client.ToolKind, e *HookEvent) string {
	switch kind {
	case client.ToolShell:
		// The command IS the operation. It falls through to the invocation only
		// for a degenerate call carrying no command, where collapsing every such
		// call onto one activity would over-grant.
		if op := client.OperationForCommand(e.command()); op != "" {
			return op
		}
	case client.ToolMCP:
		// The server and function are already structural fields of the key, so
		// the arguments are all that is left to distinguish. No fallback: an
		// empty argument set is a legitimate, stable identity, and falling back
		// to the invocation would leave a no-argument MCP call unable to survive
		// a retry.
		return client.OperationForArgs(e.ToolInput)
	}
	// No structural discriminator: fall back to the invocation, which is what
	// this key has always been for these classes. A gated class must never
	// reach here — see TestHighRiskClassesHaveAStableOperationID.
	return capStr(e.ToolUseID)
}

// toolMetadata builds the Pre/PostToolUse metadata: the permission mode plus
// the structural correlation ids. Identifiers only, never content (INV-2) —
// tool_input and tool_response are not represented.
//
// The one exception is subagent_type on a Task call, which is an identifier
// naming WHICH agent kind was spawned. Without it every subagent spawn reads
// `tool_name: Task` and the whole delegation tree is anonymous until the
// subagent's own events start arriving with an agent_id. The Task input's other
// fields — `prompt` and `description` — are free text and stay unread.
func toolMetadata(e *HookEvent) map[string]any {
	return compact(map[string]any{
		"permission_mode": enumOr(e.PermissionMode, permissionModes),
		"tool_use_id":     capStr(e.ToolUseID),
		"agent_id":        capStr(e.AgentID),
		"agent_type":      capStr(e.AgentType),
		"subagent_type":   capStr(e.subagentType()),
	})
}

// subagentMetadata returns the subagent correlation ids for a lifecycle event.
// They ride every payload fired inside a subagent, so carrying them on the
// non-tool events too keeps a session's tree complete without inventing a
// lifecycle type for the Subagent* boundary markers (see COVERAGE.md §3.2).
func subagentMetadata(e *HookEvent) map[string]any {
	return compact(map[string]any{
		"agent_id":   capStr(e.AgentID),
		"agent_type": capStr(e.AgentType),
	})
}

// mergeMetadata folds src into dst (dst wins on collision) and returns dst.
// Both maps come from compact(), so only known-present keys are copied.
func mergeMetadata(dst, src map[string]any) map[string]any {
	for k, v := range src {
		if _, exists := dst[k]; !exists {
			dst[k] = v
		}
	}
	return dst
}

// classifyTool maps a Claude Code tool name to the provider-agnostic tool
// class and the intended openbox-core span semantic type. Only three kinds
// exist in the contract (shell|file|mcp); "shell" is the catch-all for
// command-like agent tools that are neither a file operation nor an MCP
// call (WebFetch, Task, TodoWrite, …). The true tool name is always
// preserved on tool.name + metadata.tool_name, so no identity is lost by
// the coarse kind (a tool name is an identifier, not content).
//
// The semantic type is an INTENT/hint: openbox-core recomputes it server-side
// from the span name + attributes (verified — governance_workflow.go:309), and
// the client's classificationHints already sets the fields core reads. For file
// ops the client turns semantic_type=file_* + a file_path into core's file
// classification; for MCP it sets attributes["mcp.method"]="callTool".
func classifyTool(name string) (kind client.ToolKind, sem, fileOp, mcpServer, function string) {
	if strings.HasPrefix(name, "mcp__") {
		server, fn := splitMCPName(name)
		if server == "" {
			// Malformed MCP name (e.g. "mcp__"): kind=mcp with an empty
			// mcp_server is non-conformant (SL-1 requires it), so fall back to
			// the shell/internal catch-all (F5). Claude Code never emits this.
			return client.ToolShell, "internal", "", "", ""
		}
		return client.ToolMCP, "mcp_tool_call", "", server, fn
	}
	if c, ok := builtinTools[name]; ok {
		return c.kind, c.sem, c.fileOp, "", ""
	}
	// Unknown/other built-ins (WebFetch, WebSearch, Task, TodoWrite, …): a
	// command-like agent action with no dedicated core semantic type.
	return client.ToolShell, "internal", "", "", ""
}

type toolClass struct {
	kind   client.ToolKind
	sem    string
	fileOp string
}

// builtinTools classifies Claude Code's built-in tools. Anything not listed
// falls through to shell/internal (classifyTool).
var builtinTools = map[string]toolClass{
	"Write":        {client.ToolFile, "file_write", "write"},
	"Edit":         {client.ToolFile, "file_write", "edit"},
	"MultiEdit":    {client.ToolFile, "file_write", "edit"},
	"NotebookEdit": {client.ToolFile, "file_write", "edit"},
	"Read":         {client.ToolFile, "file_read", "read"},
	"NotebookRead": {client.ToolFile, "file_read", "read"},
	// Glob/Grep read the filesystem but target no single path → no file_path, so
	// core resolves them to "internal" (honest: not a single-file op).
	"Glob": {client.ToolFile, "internal", ""},
	"Grep": {client.ToolFile, "internal", ""},
	// Shell command execution — core has no shell semantic type → "internal".
	"Bash":       {client.ToolShell, "internal", ""},
	"BashOutput": {client.ToolShell, "internal", ""},
	"KillShell":  {client.ToolShell, "internal", ""},
}

// isFileSemantic reports whether a semantic type is one of core's file_* types,
// for which a file_path should accompany the span.
func isFileSemantic(sem string) bool {
	switch sem {
	case "file_read", "file_write", "file_open", "file_delete":
		return true
	}
	return false
}

// splitMCPName parses Claude Code's mcp__<server>__<tool> naming into the server
// and tool (function) names. A malformed name yields the remainder as the server
// with an empty function.
func splitMCPName(name string) (server, function string) {
	rest := strings.TrimPrefix(name, "mcp__")
	parts := strings.SplitN(rest, "__", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return parts[0], ""
}

// sessionStartMetadata builds the SessionStarted metadata (MAPPING.md §2):
// provider + the structural session facts Claude Code exposes. No content.
func sessionStartMetadata(e *HookEvent) map[string]any {
	return compact(map[string]any{
		"provider":        provider,
		"source":          enumOr(e.Source, sourceValues), // startup|resume|clear|compact
		"model":           capStr(e.Model),                // free-form model id → bounded
		"cwd":             capStr(e.Cwd),                  // structural (blessed by SL-1 testdata); not content
		"permission_mode": enumOr(e.PermissionMode, permissionModes),
	})
}

// maxIdentLen bounds every externally-influenced identifier/path field
// before egress: a crafted payload or a malicious MCP server's tool name
// can't push an unbounded / content-shaped string into tool.name,
// span.function, span.mcp_server, or file_path. These remain
// identifier-class values, never content (INV-2), but they should still be
// bounded at the untrusted boundary.
const maxIdentLen = 512

// Known Claude Code lifecycle enum values. A value outside its set is
// dropped rather than egressed verbatim, keeping metadata clean.
// apiErrorTypes is Claude Code's own StopFailure error enum, verified verbatim
// against the input schema embedded in the installed 2.1.229 binary (probe
// report §Q2). It is an allowlist, not a hint: enumOr drops anything outside it,
// which is what keeps PostToolUseFailure's free-text `error` — the same JSON key
// — from ever reaching an event.
var apiErrorTypes = map[string]bool{
	"authentication_failed": true,
	"oauth_org_not_allowed": true,
	"billing_error":         true,
	"rate_limit":            true,
	"overloaded":            true,
	"invalid_request":       true,
	"model_not_found":       true,
	"server_error":          true,
	"max_output_tokens":     true,
	"unknown":               true,
}

var (
	sourceValues    = map[string]bool{"startup": true, "resume": true, "clear": true, "compact": true}
	reasonValues    = map[string]bool{"clear": true, "resume": true, "logout": true, "prompt_input_exit": true, "bypass_permissions_disabled": true, "other": true}
	permissionModes = map[string]bool{"default": true, "plan": true, "acceptEdits": true, "auto": true, "dontAsk": true, "bypassPermissions": true}
)

// enumOr returns v when it is a recognized enum value, else "" (which compact
// then drops).
func enumOr(v string, allowed map[string]bool) string {
	if allowed[v] {
		return v
	}
	return ""
}

// capStr rune-safely bounds an identifier/path to maxIdentLen (G_SEC F1).
func capStr(s string) string {
	r := []rune(s)
	if len(r) <= maxIdentLen {
		return s
	}
	return string(r[:maxIdentLen])
}

// compact drops empty-string values so metadata carries only what is known
// (absent-when-unknown, matching the contract's omitempty posture). It always
// returns a non-nil map so lifecycle events keep a stable metadata object.
func compact(m map[string]any) map[string]any {
	for k, v := range m {
		if s, ok := v.(string); ok && s == "" {
			delete(m, k)
		}
	}
	return m
}

func (m Mapper) clock() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// redact applies the injected content redactor, or returns the text unchanged
// when none is wired (secret detection off — the honest degradation).
// gatedToolOutput wraps a tool's produced text as gated content: the org's
// opt-in first, the redactor second, attachment last (the client caps it).
//
// The ORDER is the control, not a detail. A redaction applied after attachment
// passes every unit test in this package and still ships the secret, which is
// why the conformance cases assert on the outbound bytes. Returns nil — no
// Content at all — when capture is off or nothing survives, so the absent state
// is genuinely absent rather than an empty string on the wire.
func (m Mapper) gatedToolOutput(text string) *client.Content {
	if !m.CaptureContent || text == "" {
		return nil
	}
	out := m.redact(text)
	if out == "" {
		return nil
	}
	return &client.Content{ToolOutput: out}
}

// gatedSignalDetail is gatedToolOutput's counterpart for a lifecycle signal's
// free text. Same three steps in the same order — opt-in, redact, attach — and
// the same nil-when-empty contract.
func (m Mapper) gatedSignalDetail(text string) *client.Content {
	if !m.CaptureContent || text == "" {
		return nil
	}
	out := m.redact(text)
	if out == "" {
		return nil
	}
	return &client.Content{SignalDetail: out}
}

func (m Mapper) redact(s string) string {
	if m.RedactContent == nil {
		return s
	}
	return m.RedactContent(s)
}

// eventID resolves the event's idempotency id (INV-5): the injected NewID when a
// caller/test pins one, otherwise the deterministic derivation from the event's
// own structural fields.
func (m Mapper) eventID(ev client.DevEvent) string {
	if m.NewID != nil {
		return m.NewID()
	}
	return deriveID(ev)
}

// deriveID computes the deterministic, collision-safe idempotency id
// (INV-5) for an event as "cc-" + sha256 over its structural fields. It is
// a pure function of the event, so:
//   - the same logical event always yields the same id — robust even if
//     the id is ever recomputed from the spooled/persisted record (the
//     fields it hashes all survive the spool round-trip), and
//   - two distinct events never collide: the high-resolution timestamp
//     (RFC3339Nano) is the per-event distinguisher, reinforced by the
//     structural separators (session, type, tool name, file/function
//     locator).
//
// INV-1: only non-secret structural fields feed the hash — never the obx_
// key or the Ed25519 seed (neither reaches the Mapper). INV-2: the span
// file_path is a structural locator, not content; no prompt/command/output
// text is ever hashed. INV-3: the hot-path cost is one SHA-256 over a short
// string — no I/O, no secret, allocation-cheap — so the fail-open budget is
// unchanged.
//
// Note: a stable+unique client id is only half the idempotency contract.
// The completing half is server-side dedupe on this id (carried in
// metadata.event_id and the Idempotency-Key header); openbox-core does not
// dedupe developer events on it today. Until it does, a client retry after
// a lost 200 can still be stored twice server-side — the client guarantees
// the id is stable so that eventual dedupe is trivially correct.
func deriveID(ev client.DevEvent) string {
	// 0x1f (unit separator) delimits fields so no concatenation of two events'
	// values can alias a third ("a"+"bc" hashes differently from "ab"+"c").
	const sep = 0x1f
	var b strings.Builder
	b.WriteString(ev.SessionID)
	b.WriteByte(sep)
	b.WriteString(string(ev.EventType))
	b.WriteByte(sep)
	b.WriteString(ev.Tool.Name)
	b.WriteByte(sep)
	b.WriteString(ev.Timestamp) // RFC3339Nano — the per-event distinguisher
	if ev.Span != nil {
		// The file locator / MCP function further separate two same-instant tool
		// events (structural identifiers only — INV-2).
		b.WriteByte(sep)
		b.WriteString(ev.Span.FilePath)
		b.WriteByte(sep)
		b.WriteString(ev.Span.Function)
		// The invocation id is what actually separates two same-instant calls of
		// the SAME tool. It used to arrive here via Function; splitting the two
		// identities took it out, and without it those two calls would derive one
		// event_id — the idempotency key (INV-5) — so the server would dedupe the
		// second away as a replay of the first.
		b.WriteByte(sep)
		b.WriteString(ev.Span.InvocationID)
	}
	// Turn events carry no Span, so the separators above cannot distinguish them.
	// Without these two, a main-thread turn and a subagent's turn closing in the
	// same nanosecond would derive ONE event_id — the idempotency key — and a
	// server that dedupes on it would drop one of the two as a replay.
	//
	// Appended only when set, so every existing event's id is byte-identical to
	// what it was before turn events existed (the golden fixtures pin it).
	if ev.TurnIndex != nil {
		b.WriteByte(sep)
		b.WriteString(strconv.Itoa(*ev.TurnIndex))
	}
	if ev.AgentID != "" {
		b.WriteByte(sep)
		b.WriteString(ev.AgentID)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "cc-" + hex.EncodeToString(sum[:])
}

// randomID is a source of filesystem-unique suffixes for the spool (rotate /
// recovery / reclaim file names). It is NOT the event idempotency id (that is
// deriveID); it must stay random so concurrent drains never collide on a name.
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Non-fatal: fall back to a fixed marker (a crypto/rand failure is
		// astronomically unlikely); a colliding suffix only risks losing the race
		// for one orphaned spool file, never a tool call.
		return "sfx-fallback"
	}
	return hex.EncodeToString(b[:])
}
