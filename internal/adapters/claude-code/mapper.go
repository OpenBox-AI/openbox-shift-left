package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

const provider = "claude-code"

const agentToolName = provider

// Identity is the developer-agent identity the adapter emits under. Only the
// DID is needed to build events; the obx_ key and Ed25519 seed live in the
// client, never here (INV-1).
type Identity struct {
	DeveloperDID string // did:aip:<uuid>
}

// Mapper translates Claude Code hook payloads into normalized DevEvents.
type Mapper struct {
	Identity Identity
	Now      func() time.Time // injectable clock; defaults to time.Now
	// NewID, when non-nil, overrides the idempotency-id source (INV-5); used by
	// tests to pin ids.
	NewID func() string
	// Finops, when non-nil, carries the usage numbers only the finops reader
	// extracted from the SessionEnd transcript.
	Finops *FinopsUsage
	// CaptureContent authorizes copying the (content) prompt text onto the
	// emitted PromptSubmitted event. Default false = metadata-only (INV-2): the
	// prompt is never egressed.
	CaptureContent bool
	// RedactContent redacts a content body for secrets before it is attached to
	// an event. Nil ⇒ identity, which is the honest `secret_detection:false`
	// case: the text egresses unredacted (that decision says so rather than
	// hiding it).
	RedactContent func(string) string
	// Posture, when non-nil, is the session's effective posture (E8-S5), attached
	// to the SessionStarted event's metadata only.
	Posture *devconfig.Posture
	// Evidence, when non-nil, records how much of this session's telemetry is
	// known to be undelivered at session end (E8-S7).
	Evidence *EvidenceState
	// Run, when non-nil, is this hook invocation's resolved continue-as-new
	// identity (phase 08): hookrun.go resolves it once, from the run record,
	// before New()/Record() runs, and every event Map/MapTurn emits is stamped
	// from it -- never derived here, because a minted run id cannot be
	// recomputed from anything an event carries.
	Run *RunIdentity
}

// RunIdentity is one hook invocation's resolved continue-as-new identity:
// generation 0 (the zero value) means "no continuation has ever happened for
// this session", in which case the wire run_id falls back to the session id
// (client.runIDFor) -- Run being nil on the Mapper means exactly the same
// thing, so an adapter that never wires this seam (or a hook that hit R6's
// fail-open path) emits byte-identical generation-0 events.
type RunIdentity struct {
	Generation int
	// RunID is the minted run id; empty at generation 0.
	RunID string
	// ContinuedFrom is the run this one continues from; set only when
	// Generation >= 1, and only read by the HookSessionStart case (see Map).
	ContinuedFrom string
}

// EvidenceState is a session's telemetry completeness. An alias, not a copy.
type EvidenceState = hookflow.EvidenceState

// FinopsUsage is the numbers-only usage rollup the finops reader produces from
// a transcript.
type FinopsUsage struct {
	Tokens *client.Tokens
	Cost   *client.Cost
}

// NewMapper returns a Mapper with production defaults.
func NewMapper(id Identity) Mapper {
	return Mapper{Identity: id, Now: time.Now}
}

// Map converts one hook payload into a normalized DevEvent. The bool reports
// whether an event should be emitted at all: it is false when the payload is
// unusable (no session id, or no valid developer DID); in which case the
// caller drops it fail-open (INV-3), never blocking the tool call.
func (m Mapper) Map(hook HookName, e *HookEvent) (client.DevEvent, bool) {
	if e == nil || e.SessionID == "" {
		return client.DevEvent{}, false
	}
	if !strings.HasPrefix(m.Identity.DeveloperDID, "did:aip:") {
		return client.DevEvent{}, false
	}

	now := m.clock()
	ts := now.UTC().Format(time.RFC3339Nano)

	ev := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		SessionID:     e.SessionID,
		DeveloperDID:  m.Identity.DeveloperDID,
		Timestamp:     ts,
	}
	if m.Run != nil {
		ev.RunID = m.Run.RunID
		ev.RunGeneration = m.Run.Generation
	}

	switch hook {
	case HookSessionStart:
		ev.EventType = client.EventSessionStarted
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = mergeMetadata(
			sessionStartMetadata(e),
			// Silent on any failure: an optional attribution field must never stop a
			// session reporting.
			accountMetadata(localAccount(homeDir())))
		if m.Posture != nil {
			ev.Metadata["posture"] = m.Posture.Metadata()
		}
		// The only case that sets it (phase 08 R2): lineage is a property of
		// the boundary, not of every row.
		if m.Run != nil && m.Run.ContinuedFrom != "" {
			ev.ContinuedFromRunID = m.Run.ContinuedFrom
		}

	case HookUserPromptSubmit:
		ev.EventType = client.EventPromptSubmitted
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = mergeMetadata(
			compact(map[string]any{"permission_mode": enumOr(e.PermissionMode, permissionModes)}),
			subagentMetadata(e))
		// Default off ⇒ Content stays nil and the prompt never egresses (Emit would
		// strip it anyway).
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
		if m.CaptureContent {
			// It also coupled two unrelated numbers: changing MaxCommandLen for local-
			// matching reasons silently changed what the server can see.
			if in := m.redact(toolInputExtract(e, nil)); in != "" {
				ev.Content = &client.Content{ToolInput: in}
			}
		}

	case HookPostToolUse:
		ev.EventType = client.EventToolResult
		ev.EndedAt = ts
		ev.Tool, ev.Span = mapTool(e, "completed")
		ev.Metadata = toolMetadata(e)
		ev.Status = client.StatusCompleted
		ev.Content = m.gatedToolOutput(e.toolOutputText())

	case HookPostToolUseFailure:
		ev.EventType = client.EventToolResult
		ev.EndedAt = ts
		ev.Tool, ev.Span = mapTool(e, "completed")
		ev.Metadata = toolMetadata(e)
		ev.Status = client.StatusFailed
		if e.IsInterrupt != nil {
			ev.Metadata["is_interrupt"] = *e.IsInterrupt
		}
		ev.Content = m.gatedToolOutput(e.ErrorType)

	case HookSubagentStart:
		ev.EventType = client.EventSubagentStarted
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = subagentMetadata(e)

	case HookPermissionDenied:
		// The provider's free-text `reason` rides Content.SignalDetail, which the
		// client keys as metadata.denial_reason -- and, since v1.9, projects into
		// signal_args too, where a policy can actually match it. It used to be
		// barred from signal_args entirely; core's goal gate is what changed that,
		// not a change of mind. See conformance C38.
		ev.EventType = client.EventPermissionDenied
		ev.Tool, ev.Span = mapTool(e, "completed")
		ev.Metadata = toolMetadata(e)
		ev.Content = m.gatedSignalDetail(e.Reason)

	case HookStopFailure:
		ev.EventType = client.EventAPIError
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = mergeMetadata(
			compact(map[string]any{"error_type": enumOr(e.ErrorType, apiErrorTypes)}),
			subagentMetadata(e))
		ev.Content = m.gatedSignalDetail(e.ErrorDetails)

	case HookSessionEnd:
		ev.EventType = client.EventSessionEnded
		ev.EndedAt = ts
		ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
		ev.Metadata = compact(map[string]any{"reason": enumOr(e.Reason, reasonValues)})
		if m.Finops != nil {
			ev.Tokens = m.Finops.Tokens
			ev.Cost = m.Finops.Cost
		}
		if m.Evidence != nil {
			ev.Metadata = mergeMetadata(ev.Metadata, m.Evidence.Metadata())
		}

	// v1.8 observe-only lifecycle signals (21 classes, phase-04 table B
	// order). Every case below goes through signalEvent: the agent as the
	// tool, structural metadata, and no activity fields at all (insight 1) —
	// no new case may set ev.Span, ev.StartedAt, ev.EndedAt, ev.TurnIndex,
	// ev.Status, ev.ActivityType or ev.Tokens.

	case HookSetup:
		m.signalEvent(&ev, client.EventSetup, compact(map[string]any{
			"trigger": enumOr(e.Trigger, setupTriggers),
		}))

	case HookInstructionsLoaded:
		meta := compact(map[string]any{
			"file_path":         capStr(e.FilePath),
			"memory_type":       enumOr(e.MemoryType, memoryTypes),
			"load_reason":       enumOr(e.LoadReason, loadReasons),
			"trigger_file_path": capStr(e.TriggerFilePath),
			"parent_file_path":  capStr(e.ParentFilePath),
		})
		if len(e.Globs) > 0 {
			n := len(e.Globs)
			if n > maxInstructionGlobs {
				n = maxInstructionGlobs
			}
			globs := make([]string, 0, n)
			for _, g := range e.Globs[:n] {
				globs = append(globs, capStr(g))
			}
			meta["globs"] = globs
		}
		m.signalEvent(&ev, client.EventInstructionsLoaded, meta)

	case HookUserPromptExpansion:
		// No content line (structural-only, A-04 A1). command_args is unbound,
		// and the pre-expansion `prompt` is unbound because it duplicates
		// prompt_submitted under a second key — the reason matters, or
		// someone re-adds it.
		m.signalEvent(&ev, client.EventUserPromptExpansion, compact(map[string]any{
			"expansion_type": enumOr(e.ExpansionType, expansionTypes),
			"command_name":   capStr(e.CommandName),
			// command_source: capStr, NOT enumOr (R1) — only one documented
			// value and no confirmed table; an unconfirmed allowlist would
			// silently discard real data.
			"command_source": capStr(e.CommandSource),
		}))

	case HookMessageDisplay:
		// D1: no content line. delta and displayContent are never bound, and
		// message_id is not the API msg_… id, so no transcript join exists.
		meta := compact(map[string]any{
			"turn_id":    capStr(e.TurnID),
			"message_id": capStr(e.MessageID),
		})
		if e.Index != nil {
			meta["index"] = *e.Index
		}
		if e.Final != nil {
			meta["final"] = *e.Final
		}
		m.signalEvent(&ev, client.EventMessageDisplay, meta)

	case HookPermissionRequest:
		// No tool_use_id exists, so this cannot pair with any tool activity;
		// permission_suggestions not bound (R3). C7: after phase 07 this key
		// carries the whole subagent prompt for an Agent request.
		m.signalEvent(&ev, client.EventPermissionRequest, compact(map[string]any{
			"tool_name":       capStr(e.ToolName),
			"permission_mode": enumOr(e.PermissionMode, permissionModes),
		}))
		ev.Content = m.gatedSignalDetail(toolInputExtract(e, nil))

	case HookPostToolBatch:
		// D1: no content line. tool_calls[] is span-shaped and no event
		// carries spans[].
		ids := e.batchToolUseIDs()
		meta := map[string]any{"batch_size": len(ids)}
		if len(ids) > 0 {
			meta["batch_tool_use_ids"] = ids
		}
		m.signalEvent(&ev, client.EventPostToolBatch, meta)

	case HookNotification:
		notif := compact(map[string]any{
			"notification_type": enumOr(e.NotificationType, notificationTypes),
		})
		m.gatedContentMeta(notif, "notification_title", e.Title)
		m.signalEvent(&ev, client.EventNotification, notif)
		ev.Content = m.gatedSignalDetail(e.Message)

	case HookTaskCreated, HookTaskCompleted:
		// Two signals, never an Activity pair: a task can be abandoned or
		// complete in another session, and core increments `total` on
		// Started and success/fail only on Completed, so an orphan pair
		// would depress success rates. R2: teammate_name/team_name are
		// structural, same capStr treatment as agent_type.
		et := client.EventTaskCreated
		if hook == HookTaskCompleted {
			et = client.EventTaskCompleted
		}
		task := compact(map[string]any{
			"task_id":       capStr(e.TaskID),
			"teammate_name": capStr(e.TeammateName),
			"team_name":     capStr(e.TeamName),
		})
		m.gatedContentMeta(task, "task_description", e.TaskDescription)
		m.signalEvent(&ev, et, task)
		ev.Content = m.gatedSignalDetail(e.TaskSubject)

	case HookTeammateIdle:
		m.signalEvent(&ev, client.EventTeammateIdle, compact(map[string]any{
			"teammate_name": capStr(e.TeammateName),
			"team_name":     capStr(e.TeamName),
		}))

	case HookConfigChange:
		// No content: `reason` is an output field the gate renders locally
		// (phase 09) and never egresses.
		m.signalEvent(&ev, client.EventConfigChange, compact(map[string]any{
			"source":    enumOr(e.Source, configSources),
			"file_path": capStr(e.FilePath),
		}))

	case HookCwdChanged:
		m.signalEvent(&ev, client.EventCwdChanged, compact(map[string]any{
			"old_cwd": capStr(e.OldCwd),
			"new_cwd": capStr(e.NewCwd),
		}))

	case HookDirectoryAdded:
		m.signalEvent(&ev, client.EventDirectoryAdded, compact(map[string]any{
			"directory": capStr(e.Directory),
			"source":    enumOr(e.Source, directoryAddedSources),
		}))

	case HookFileChanged:
		m.signalEvent(&ev, client.EventFileChanged, compact(map[string]any{
			"file_path": capStr(e.FilePath),
			"event":     enumOr(e.FileChangeEvent, fileChangeEvents),
		}))

	case HookWorktreeRemove:
		// Unpaired by construction: it correlates to WorktreeCreate, which
		// this adapter refuses to register.
		m.signalEvent(&ev, client.EventWorktreeRemove, compact(map[string]any{
			"worktree_path": capStr(e.WorktreePath),
		}))

	case HookPreCompact:
		m.signalEvent(&ev, client.EventPreCompact, compact(map[string]any{
			"trigger": enumOr(e.Trigger, compactTriggers),
		}))
		ev.Content = m.gatedSignalDetail(e.CustomInstructions)

	case HookPostCompact:
		m.signalEvent(&ev, client.EventPostCompact, compact(map[string]any{
			"trigger": enumOr(e.Trigger, compactTriggers),
		}))
		ev.Content = m.gatedSignalDetail(e.CompactSummary)

	case HookPreModelSwitch:
		// Never set ev.Model: buildMetadata copies ev.Model into
		// metadata.model, the key core aggregates token rollups under. A
		// switch spends no tokens; from_model/to_model are their own keys.
		m.signalEvent(&ev, client.EventPreModelSwitch, modelSwitchMetadata(e, preModelSwitchSources))

	case HookPostModelSwitch:
		// Same shape as PreModelSwitch, postModelSwitchSources only:
		// declared in full rather than derived from preModelSwitchSources so
		// the two constants agreeing today stays a visible coincidence, not
		// a hidden derivation (phase 09 asserts pre ⊆ post). Never set
		// ev.Model, for the same reason as PreModelSwitch.
		m.signalEvent(&ev, client.EventPostModelSwitch, modelSwitchMetadata(e, postModelSwitchSources))

	case HookElicitation:
		// requested_schema not bound (R3).
		m.signalEvent(&ev, client.EventElicitation, compact(map[string]any{
			"mcp_server_name": capStr(e.MCPServerName),
			"mode":            enumOr(e.Mode, elicitationModes),
			"url":             capStr(e.URL),
			"elicitation_id":  capStr(e.ElicitationID),
		}))
		ev.Content = m.gatedSignalDetail(e.Message)

	case HookElicitationResult:
		// D2: form values are ordinary gated content; the residual risk
		// lives in docs/data-and-privacy.md.
		m.signalEvent(&ev, client.EventElicitationResult, compact(map[string]any{
			"mcp_server_name": capStr(e.MCPServerName),
			"action":          enumOr(e.Action, elicitationActions),
			"mode":            enumOr(e.Mode, elicitationModes),
			"elicitation_id":  capStr(e.ElicitationID),
		}))
		ev.Content = m.gatedSignalDetail(e.elicitationContentText())

	case HookStop, HookSubagentStop:
		return client.DevEvent{}, false

	default:
		return client.DevEvent{}, false
	}

	if ev.Metadata == nil {
		ev.Metadata = map[string]any{}
	}
	ev.Metadata = mergeMetadata(ev.Metadata, commonMetadata(e))

	ev.EventID = m.eventID(ev)
	return ev, true
}

// signalEvent is the shape every observe-only lifecycle signal shares: the
// agent as the tool, structural metadata, and no activity fields at all.
func (m Mapper) signalEvent(ev *client.DevEvent, t client.EventType, meta map[string]any) {
	ev.EventType = t
	ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
	ev.Metadata = meta
}

// commonMetadata is what every payload carries regardless of event. prompt_id
// is absent until the first user input and on any pre-prompt event, so
// compact() drops it and those events serialize exactly as they did.
func commonMetadata(e *HookEvent) map[string]any {
	return compact(map[string]any{"prompt_id": capStr(e.PromptID)})
}

// MapTurn builds one model turn's ActivityStarted/ActivityCompleted pair from
// a Stop/SubagentStop firing and the transcript window it delimits.
func (m Mapper) MapTurn(e *HookEvent, w turnWindow, index int) (started, completed client.DevEvent, ok bool) {
	if e == nil || e.SessionID == "" || !w.HasUsage {
		return client.DevEvent{}, client.DevEvent{}, false
	}
	if !strings.HasPrefix(m.Identity.DeveloperDID, "did:aip:") {
		return client.DevEvent{}, client.DevEvent{}, false
	}

	now := m.clock()
	closeTS := now.UTC().Format(time.RFC3339Nano)
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
	if m.Run != nil {
		base.RunID = m.Run.RunID
		base.RunGeneration = m.Run.Generation
	}

	started = base
	started.EventType = client.EventTurnStarted
	started.Timestamp = openTS
	started.StartedAt = openTS
	started.Metadata = turnMetadata(e, turnIndex)
	started.Metadata = mergeMetadata(started.Metadata, commonMetadata(e))
	started.EventID = m.eventID(started)

	completed = base
	completed.EventType = client.EventTurnCompleted
	completed.Timestamp = closeTS
	completed.EndedAt = closeTS
	if haveRealOpen {
		completed.StartedAt = openTS
	}
	completed.Tokens = w.tokens()
	completed.Model = capStr(w.Model)
	// Deliberately NOT on the started half: a turn's input is the prompt, which
	// already rides PromptSubmitted under the same gate.
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
	completed.Metadata = mergeMetadata(completed.Metadata, commonMetadata(e))
	completed.EventID = m.eventID(completed)

	return started, completed, true
}

// turnMetadata identifiers and one integer, never content (INV-2).
func turnMetadata(e *HookEvent, index int) map[string]any {
	m := compact(map[string]any{
		"agent_id":   capStr(e.AgentID),
		"agent_type": capStr(e.AgentType),
	})
	m["turn_index"] = index
	return m
}

// mapTool two identities, deliberately separate (client.Span.InvocationID /
// OperationID):
//   - InvocationID = tool_use_id, which Claude Code mints per call.
//   - Shell → the command, hashed.
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

func operationID(kind client.ToolKind, e *HookEvent) string {
	switch kind {
	case client.ToolShell:
		if op := client.OperationForCommand(e.command()); op != "" {
			return op
		}
	case client.ToolMCP:
		return client.OperationForArgs(e.ToolInput)
	}
	// A gated class must never reach here; see
	// TestHighRiskClassesHaveAStableOperationID.
	return capStr(e.ToolUseID)
}

// toolMetadata identifiers only, never content (INV-2); tool_input and
// tool_response are not represented.
func toolMetadata(e *HookEvent) map[string]any {
	return compact(map[string]any{
		"permission_mode": enumOr(e.PermissionMode, permissionModes),
		"tool_use_id":     capStr(e.ToolUseID),
		"agent_id":        capStr(e.AgentID),
		"agent_type":      capStr(e.AgentType),
		"subagent_type":   capStr(e.subagentType()),
	})
}

func subagentMetadata(e *HookEvent) map[string]any {
	return compact(map[string]any{
		"agent_id":   capStr(e.AgentID),
		"agent_type": capStr(e.AgentType),
	})
}

func mergeMetadata(dst, src map[string]any) map[string]any {
	for k, v := range src {
		if _, exists := dst[k]; !exists {
			dst[k] = v
		}
	}
	return dst
}

func classifyTool(name string) (kind client.ToolKind, sem, fileOp, mcpServer, function string) {
	if strings.HasPrefix(name, "mcp__") {
		server, fn := splitMCPName(name)
		if server == "" {
			// Claude Code never emits this.
			return client.ToolShell, "internal", "", "", ""
		}
		return client.ToolMCP, "mcp_tool_call", "", server, fn
	}
	if c, ok := builtinTools[name]; ok {
		return c.kind, c.sem, c.fileOp, "", ""
	}
	return client.ToolShell, "internal", "", "", ""
}

type toolClass struct {
	kind   client.ToolKind
	sem    string
	fileOp string
}

var builtinTools = map[string]toolClass{
	"Write":        {client.ToolFile, "file_write", "write"},
	"Edit":         {client.ToolFile, "file_write", "edit"},
	"MultiEdit":    {client.ToolFile, "file_write", "edit"},
	"NotebookEdit": {client.ToolFile, "file_write", "edit"},
	"Read":         {client.ToolFile, "file_read", "read"},
	"NotebookRead": {client.ToolFile, "file_read", "read"},
	"Glob":         {client.ToolFile, "internal", ""},
	"Grep":         {client.ToolFile, "internal", ""},
	"Bash":         {client.ToolShell, "internal", ""},
	"BashOutput":   {client.ToolShell, "internal", ""},
	"KillShell":    {client.ToolShell, "internal", ""},

	// Agent delegates unbounded work to a fresh subagent and, unmapped, reached
	// core as {"kind":"shell","tool_name":"Agent"} -- name only, zero judgements
	// on a measured session (phase 07). Kind stays ToolShell so the local
	// enforce gate (which shares classifyTool) is unchanged; the "llm_tool_call"
	// semantic is what routes toolInputExtract and contentKeyFor away from the
	// shell-command path, so the spawn's whole tool_input (prompt included)
	// egresses under `arguments`, never `command`.
	"Agent": {client.ToolShell, "llm_tool_call", ""},

	// ToolSearch gets the same treatment (ruling 3: query uncapped beyond the
	// content gate). It does fire PreToolUse -- measured, not assumed: the
	// provider exempts exactly one tool (EndConversation) from the Pre/PostToolUse
	// runners, and this machine's transcripts hold 93 PreToolUse:ToolSearch hook
	// runs produced by this adapter, each carrying an ALLOW verdict from
	// /evaluate. So every search has been gated all along; what changes here is
	// what the judge can see, not whether the call is governed.
	"ToolSearch": {client.ToolShell, "llm_tool_call", ""},
}

func isFileSemantic(sem string) bool {
	switch sem {
	case "file_read", "file_write", "file_open", "file_delete":
		return true
	}
	return false
}

func splitMCPName(name string) (server, function string) {
	rest := strings.TrimPrefix(name, "mcp__")
	parts := strings.SplitN(rest, "__", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return parts[0], ""
}

func sessionStartMetadata(e *HookEvent) map[string]any {
	return compact(map[string]any{
		"provider":        provider,
		"source":          enumOr(e.Source, sourceValues), // startup|resume|clear|compact|fork
		"model":           capStr(e.Model),                // free-form model id → bounded
		"cwd":             capStr(e.Cwd),                  // structural (blessed by SL-1 testdata); not content
		"permission_mode": enumOr(e.PermissionMode, permissionModes),
	})
}

// maxIdentLen bounds every externally-influenced identifier/path field before
// egress: a crafted payload or a malicious MCP server's tool name can't push
// an unbounded / content-shaped string into tool.name, span.function,
// span.mcp_server, or file_path.
const maxIdentLen = 512

// maxInstructionGlobs caps InstructionsLoaded's globs[] so one payload cannot
// grow the metadata blob without bound.
const maxInstructionGlobs = 32

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
	// sourceValues is SessionStart's `source` enum only (insight 2: enumOr
	// must be per hook, never reused across the four events that carry a
	// `source` field). Phase 08's isBumpSource reads this list to decide
	// whether a run continues: ONLY resume bumps the run generation (resume-
	// only re-scope, live-measured against Claude Code 2.1.263 -- a `/clear`
	// mints a brand-new session id, so there is no prior run under it to
	// continue); startup/clear/compact/fork do not — a missing value here is
	// a wrong run identity, not a cosmetic metadata gap (B-06 R5 added
	// "fork").
	sourceValues = map[string]bool{"startup": true, "resume": true, "clear": true, "compact": true, "fork": true}
	// reasonValues keeps bypass_permissions_disabled even though it is absent
	// from the vendor docs and the 2.1.260 binary: enumOr would otherwise
	// silently drop a real future value, so keeping it is the
	// forward-compatible choice (B-06 R6).
	reasonValues    = map[string]bool{"clear": true, "resume": true, "logout": true, "prompt_input_exit": true, "bypass_permissions_disabled": true, "other": true}
	permissionModes = map[string]bool{"default": true, "plan": true, "acceptEdits": true, "auto": true, "dontAsk": true, "bypassPermissions": true}
)

// The 15 v1.8 per-hook allowlists below (phase 06). Each binds exactly one
// hook's enum field; none is reused across hooks that happen to share a JSON
// key name (`source`, `trigger`) — see sourceValues' comment for why that
// matters.
var (
	setupTriggers   = map[string]bool{"init": true, "maintenance": true}
	compactTriggers = map[string]bool{"manual": true, "auto": true}
	memoryTypes     = map[string]bool{"User": true, "Project": true, "Local": true, "Managed": true}
	loadReasons     = map[string]bool{
		"session_start": true, "nested_traversal": true, "path_glob_match": true,
		"include": true, "compact": true,
	}
	expansionTypes    = map[string]bool{"slash_command": true, "mcp_prompt": true}
	notificationTypes = map[string]bool{
		"permission_prompt": true, "idle_prompt": true, "auth_success": true, "elicitation_dialog": true,
		"elicitation_url_dialog": true, "elicitation_complete": true, "elicitation_response": true,
		"agent_needs_input": true, "agent_completed": true, "quota_auto_resume_fired": true,
		"quota_auto_resume_stale": true, "quota_auto_resume_disabled": true,
	}
	configSources = map[string]bool{
		"user_settings": true, "project_settings": true, "local_settings": true,
		"policy_settings": true, "skills": true,
	}
	directoryAddedSources = map[string]bool{"slash_command": true, "register_repo_root": true}
	fileChangeEvents      = map[string]bool{"change": true, "add": true, "unlink": true}
	preModelSwitchSources = map[string]bool{"command": true, "picker": true, "sdk": true}
	// postModelSwitchSources is declared in full rather than derived from
	// preModelSwitchSources: the repo's precedent for two constants that
	// agree today is to keep the coincidence visible. Phase 09 asserts
	// pre ⊆ post so a doc change that diverges them is caught.
	postModelSwitchSources = map[string]bool{
		"command": true, "picker": true, "sdk": true, "auto": true, "resume": true,
	}
	cacheTTLValues     = map[string]bool{"5m": true, "1h": true}
	pricingValues      = map[string]bool{"configured": true, "catalog": true, "default": true}
	elicitationModes   = map[string]bool{"form": true, "url": true}
	elicitationActions = map[string]bool{"accept": true, "decline": true, "cancel": true}
)

func enumOr(v string, allowed map[string]bool) string {
	if allowed[v] {
		return v
	}
	return ""
}

// capStr delegates so the adapters and the engine cap an identifier the same
// way; maxIdentLen stays declared here because tests read it.
func capStr(s string) string { return hookflow.CapIdent(s) }

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

// gatedContentMeta writes free text into a signal's metadata under the same two
// rules gatedSignalDetail applies to Content: capture must be on, and the text
// is redacted before it is attached, never after.
//
// It exists because Content.SignalDetail is ONE string and these classes have
// already spent it — notification_message on Notification, task_subject on the
// two Task classes. A second free-text field per class therefore needs another
// carrier, and metadata is the supported one: INV-2 names content-bearing
// metadata keys as an expected case with contentMetadataKeys as the backstop.
// The client caps the value on egress, where the content bound is owned.
//
// The backstop is a second line, not the gate. It only fires once an event is
// content-stripped, so a key written here without the capture check would still
// egress on a capture-on org that had opted this field out. Both are required.
//
// Empty text writes no key at all: an empty string in signal_args is a value a
// policy can match on, and "" is not a fact anyone asserted.
func (m Mapper) gatedContentMeta(meta map[string]any, key, text string) {
	if !m.CaptureContent || text == "" {
		return
	}
	if out := m.redact(text); out != "" {
		meta[key] = out
	}
}

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

func (m Mapper) eventID(ev client.DevEvent) string {
	if m.NewID != nil {
		return m.NewID()
	}
	return deriveID(ev)
}

//   - The same logical event always yields the same id; robust even if the id
//     is ever recomputed from the spooled/persisted record (the fields it
//     hashes all survive the spool round-trip), and
//   - Two distinct events never collide: the high-resolution timestamp
//     (RFC3339Nano) is the per-event distinguisher, reinforced by the
//     structural separators (session, type, tool name, file/function locator).
func deriveID(ev client.DevEvent) string {
	const sep = 0x1f
	var b strings.Builder
	b.WriteString(ev.SessionID)
	b.WriteByte(sep)
	b.WriteString(string(ev.EventType))
	b.WriteByte(sep)
	b.WriteString(ev.Tool.Name)
	b.WriteByte(sep)
	b.WriteString(ev.Timestamp) // RFC3339Nano; the per-event distinguisher
	if ev.Span != nil {
		b.WriteByte(sep)
		b.WriteString(ev.Span.FilePath)
		b.WriteByte(sep)
		b.WriteString(ev.Span.Function)
		b.WriteByte(sep)
		b.WriteString(ev.Span.InvocationID)
	}
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

// modelSwitchMetadata is the shape both model-switch classes carry; only the
// accepted source allowlist differs, and those two tables stay separately
// declared on purpose.
func modelSwitchMetadata(e *HookEvent, sources map[string]bool) map[string]any {
	meta := compact(map[string]any{
		"from_model":      capStr(e.FromModel),
		"to_model":        capStr(e.ToModel),
		"requested_model": capStr(e.RequestedModel),
		"source":          enumOr(e.Source, sources),
		"cache_ttl":       enumOr(e.CacheTTL, cacheTTLValues),
		"pricing":         enumOr(e.Pricing, pricingValues),
	})
	if e.ContextTokens != nil {
		meta["context_tokens"] = *e.ContextTokens
	}
	if e.PromptCacheWarm != nil {
		meta["prompt_cache_warm"] = *e.PromptCacheWarm
	}
	if e.EstimatedCacheWriteUSD != nil {
		meta["estimated_cache_write_usd"] = *e.EstimatedCacheWriteUSD
	}
	return meta
}
