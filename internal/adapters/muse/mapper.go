package muse

import (
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

const provider = "muse"

const agentToolName = provider

// Identity is the developer-agent identity the adapter emits under. Only the
// DID is needed to build events; the key material lives in the client, never
// here (INV-1).
type Identity = hookflow.Identity

// RunIdentity is one hook invocation's resolved continue-as-new identity:
// generation 0 (the zero value) means no continuation has happened for this
// session, and the wire run_id falls back to the session id. A nil Run on the
// Mapper means the same thing.
type RunIdentity struct {
	Generation int
	// RunID is the minted run id; empty at generation 0.
	RunID string
	// ContinuedFrom is the run this one continues from; set only when
	// Generation >= 1, and only read by the SessionStart case.
	ContinuedFrom string
}

// EvidenceState is a session's telemetry completeness. An alias, not a copy.
type EvidenceState = hookflow.EvidenceState

// Mapper translates Muse hook payloads into normalized DevEvents.
type Mapper struct {
	Identity Identity
	Now      func() time.Time // injectable clock; defaults to time.Now
	// NewID, when non-nil, overrides the idempotency-id source; used by tests to
	// pin ids.
	NewID func() string
	// CaptureContent authorizes copying content (the prompt, tool input and
	// output, model-call message previews) onto an emitted event: on by default,
	// opt-out honoured.
	CaptureContent bool
	// RedactContent redacts a content body for secrets before it is attached.
	// Nil is the identity, which is the honest secret_detection:false case.
	RedactContent func(string) string
	// Posture, when non-nil, is attached to the SessionStarted metadata only.
	Posture *devconfig.Posture
	// Evidence, when non-nil, records how much of the session's telemetry is
	// known to be undelivered at session end.
	Evidence *EvidenceState
	// Run is the session's resolved run identity.
	Run *RunIdentity
}

// NewMapper returns a Mapper with production defaults.
func NewMapper(id Identity) Mapper {
	return Mapper{Identity: id, Now: time.Now}
}

// Map converts one hook payload into a normalized DevEvent. The bool reports
// whether an event should be emitted at all: false when the payload is unusable
// (no session id, no valid developer DID) or the hook maps to nothing. The
// caller drops it; a gated caller then denies, never proceeds.
//
// A folded subagent event (foldSubagent) is mapped under its parent's session
// and tagged the way Claude Code tags a subagent's events: agent_id and
// agent_type on every event, SubagentStart as the SubagentStarted signal, and
// no SessionEnded for its SubagentStop or SessionEnd.
func (m Mapper) Map(hook HookName, e *HookEvent) (client.DevEvent, bool) {
	ev, ok := m.mapHook(hook, e)
	if ok && e.folded() {
		ev.Metadata = mergeMetadata(ev.Metadata, subagentMetadata(e))
	}
	return ev, ok
}

func (m Mapper) mapHook(hook HookName, e *HookEvent) (client.DevEvent, bool) {
	if e == nil || e.SessionID == "" || !m.Identity.HasDeveloperDID() {
		return client.DevEvent{}, false
	}

	ts := m.clock().UTC().Format(time.RFC3339Nano)
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
	agent := client.Tool{Name: agentToolName, Kind: client.ToolShell}

	switch hook {
	case HookSessionStart:
		ev.EventType = client.EventSessionStarted
		ev.Tool = agent
		ev.Metadata = sessionStartMetadata(e)
		if m.Posture != nil {
			ev.Metadata["posture"] = m.Posture.Metadata()
		}
		if m.Run != nil && m.Run.ContinuedFrom != "" {
			ev.ContinuedFromRunID = m.Run.ContinuedFrom
		}

	case HookUserPromptSubmit:
		ev.EventType = client.EventPromptSubmitted
		ev.Tool = agent
		ev.Metadata = hookflow.Compact(map[string]any{
			"permission_mode": hookflow.EnumOr(e.PermissionMode, permissionModes),
			"turn_id":         capStr(e.TurnID),
		})
		if e.folded() {
			// A subagent's prompt is not the developer's goal, and a
			// prompt_submitted signal's args are what core reads the goal off:
			// its text is withheld, as Claude Code withholds a machine-injected
			// turn's.
			ev.Metadata = mergeMetadata(ev.Metadata, map[string]any{"prompt_source": subagentAgentType})
		} else if m.CaptureContent && e.Prompt != "" {
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
			if in := m.redact(toolInputExtract(e)); in != "" {
				ev.Content = &client.Content{ToolInput: in}
			}
		}

	case HookPermissionRequest:
		// A signal, not an activity: the escalation is not a second execution of
		// the tool, and Muse gives it no tool_use_id to pair with.
		ev.EventType = client.EventPermissionRequest
		kind, _, _, mcpServer, _ := classifyTool(e.ToolName)
		ev.Tool = client.Tool{Name: capStr(e.ToolName), Kind: kind}
		if kind == client.ToolMCP {
			ev.Tool.MCPServer = capStr(mcpServer)
		}
		ev.Metadata = hookflow.Compact(map[string]any{
			"permission_mode": hookflow.EnumOr(e.PermissionMode, permissionModes),
			"tool_name":       capStr(e.ToolName),
		})

	case HookPostToolUse:
		ev.EventType = client.EventToolResult
		ev.EndedAt = ts
		ev.Tool, ev.Span = mapTool(e, "completed")
		ev.Metadata = toolMetadata(e)
		ev.Status = client.StatusCompleted
		ev.Content = m.gatedToolOutput(outputText(e.ToolResponse))

	case HookPostToolUseFailure:
		ev.EventType = client.EventToolResult
		ev.EndedAt = ts
		ev.Tool, ev.Span = mapTool(e, "completed")
		ev.Metadata = toolMetadata(e)
		ev.Status = client.StatusFailed
		ev.Content = m.gatedToolOutput(outputText(e.Error))

	case HookSubagentStart:
		if e.folded() {
			ev.EventType = client.EventSubagentStarted
			ev.Tool = agent
			ev.Metadata = subagentMetadata(e)
			break
		}
		// A subagent whose parent could not be found stays a session of its
		// own, so its start is that session's SessionStarted.
		ev.EventType = client.EventSessionStarted
		ev.Tool = agent
		ev.Metadata = mergeMetadata(sessionStartMetadata(e), subagentMetadata(e))
		if m.Posture != nil {
			ev.Metadata["posture"] = m.Posture.Metadata()
		}
		if m.Run != nil && m.Run.ContinuedFrom != "" {
			ev.ContinuedFromRunID = m.Run.ContinuedFrom
		}

	case HookSubagentStop:
		if e.folded() {
			return client.DevEvent{}, false
		}
		ev.EventType = client.EventSessionEnded
		ev.EndedAt = ts
		ev.Tool = agent
		ev.Metadata = subagentMetadata(e)
		if m.Evidence != nil {
			ev.Metadata = mergeMetadata(ev.Metadata, m.Evidence.Metadata())
		}

	case HookPreCompact, HookPostCompact:
		// Signals, unpaired: Muse fires PreCompact for a compaction that then
		// fails, and for internal sessions, so there is no PostCompact to wait
		// for and none is synthesized. trigger is Muse's own word (soft|hard).
		et := client.EventPreCompact
		if hook == HookPostCompact {
			et = client.EventPostCompact
		}
		m.signalEvent(&ev, et, hookflow.Compact(map[string]any{
			"trigger": hookflow.EnumOr(e.Trigger, compactTriggers),
		}))

	case HookNotification:
		notif := hookflow.Compact(map[string]any{
			"notification_type": hookflow.EnumOr(e.NotificationType, notificationTypes),
		})
		if m.CaptureContent && e.Title != "" {
			if t := m.redact(e.Title); t != "" {
				notif["notification_title"] = t
			}
		}
		m.signalEvent(&ev, client.EventNotification, notif)
		ev.Content = m.gatedSignalDetail(e.Message)

	case HookStopFailure:
		// The provider's error text is undocumented and never bound, so the row
		// says only that a turn ended in a provider-side error.
		ev.EventType = client.EventAPIError
		ev.Tool = agent
		ev.Metadata = hookflow.Compact(map[string]any{"turn_id": capStr(e.TurnID)})

	case HookSessionEnd:
		// A child session ending is not its parent ending: folded, it would
		// close the parent's workflow mid-session, so it maps to nothing, the
		// same as a folded SubagentStop.
		if e.folded() {
			return client.DevEvent{}, false
		}
		ev.EventType = client.EventSessionEnded
		ev.EndedAt = ts
		ev.Tool = agent
		ev.Metadata = hookflow.Compact(map[string]any{"reason": hookflow.EnumOr(e.Reason, reasonValues)})
		if m.Evidence != nil {
			ev.Metadata = mergeMetadata(ev.Metadata, m.Evidence.Metadata())
		}

	case HookPreLLMCall:
		return m.mapModelCallRequested(ev, agent, e)

	case HookPostLLMCall:
		return m.mapModelCallFinished(ev, agent, e)

	default:
		return client.DevEvent{}, false
	}

	ev.EventID = m.eventID(ev)
	return ev, true
}

func (m Mapper) signalEvent(ev *client.DevEvent, t client.EventType, meta map[string]any) {
	ev.EventType = t
	ev.Tool = client.Tool{Name: agentToolName, Kind: client.ToolShell}
	ev.Metadata = meta
}

func (m Mapper) gatedSignalDetail(text string) *client.Content {
	if out := hookflow.GatedText(m.CaptureContent, m.RedactContent, text); out != "" {
		return &client.Content{SignalDetail: out}
	}
	return nil
}

// subagentMetadata marks a session as a subagent's own: agent_id is Muse's
// subagent_id (for example "skill-reminder") and agent_type says what kind of
// session it is. The dev-event schema has no is_subagent or subagent_id
// metadata key, and these two are the existing keys that carry the same facts.
func subagentMetadata(e *HookEvent) map[string]any {
	return hookflow.Compact(map[string]any{
		"agent_id":   capStr(e.SubagentID),
		"agent_type": subagentAgentType,
	})
}

// subagentAgentType is the agent_type of every Muse subagent session.
const subagentAgentType = "subagent"

func mergeMetadata(dst, src map[string]any) map[string]any {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]any, len(src))
	}
	for k, v := range src {
		if _, exists := dst[k]; !exists {
			dst[k] = v
		}
	}
	return dst
}

// toolMetadata is identifiers only, never content (INV-2).
func toolMetadata(e *HookEvent) map[string]any {
	return hookflow.Compact(map[string]any{
		"permission_mode": hookflow.EnumOr(e.PermissionMode, permissionModes),
		"tool_use_id":     capStr(e.ToolUseID),
		"turn_id":         capStr(e.TurnID),
	})
}

// toolCall is this payload's tool invocation, classified by classifyTool so
// the observe event and the enforce gate classify a tool identically.
func toolCall(e *HookEvent) hookflow.ToolCall {
	kind, sem, fileOp, mcpServer, function := classifyTool(e.ToolName)
	return hookflow.ToolCall{
		SessionID:       e.SessionID,
		ToolName:        e.ToolName,
		ToolUseID:       e.ToolUseID,
		ToolInput:       e.ToolInput,
		PermissionMode:  e.PermissionMode,
		PermissionModes: permissionModes,
		Kind:            kind,
		Sem:             sem,
		FileOp:          fileOp,
		MCPServer:       mcpServer,
		Function:        function,
		FilePath:        e.filePath,
		Command:         e.command,
		DecisionCommand: e.shellText,
		FileText:        e.fileText,
	}
}

func mapTool(e *HookEvent, stage string) (client.Tool, *client.Span) {
	tc := toolCall(e)
	return tc.Tool(), tc.Span(stage)
}

// classifyTool classifies a tool by name against this adapter's builtin table
// (hookflow.ClassifyTool).
func classifyTool(name string) (kind client.ToolKind, sem, fileOp, mcpServer, function string) {
	return hookflow.ClassifyTool(name, builtinTools)
}

type toolClass hookflow.ToolClass

// builtinTools maps Muse's tool names. Observed on 1.4.1: bash, bash_input,
// read_file, write_file, edit_file and search are the native spellings. Muse
// also runs internal tools of its own (submit_reminder_decision, subagent_*,
// work_*, cron_*, *_memory, *_goal, write_todos, report_progress, read_skill,
// snooze_reminder, monitor, workflow) that are deliberately absent here:
// classifyTool sends any name this table does not know to shell-kinded,
// semantically opaque, and the gate still evaluates it.
//
// The Claude Code names (Bash, Read, Edit, Write, Grep, WebFetch, WebSearch)
// are documented aliases, and the remaining spellings were never observed; they
// are kept so a rename does not turn a write into an unclassified call.
var builtinTools = map[string]toolClass{
	"Bash":       {client.ToolShell, "internal", ""},
	"BashOutput": {client.ToolShell, "internal", ""},
	"KillShell":  {client.ToolShell, "internal", ""},
	"bash":       {client.ToolShell, "internal", ""},
	"bash_input": {client.ToolShell, "internal", ""},
	"shell":      {client.ToolShell, "internal", ""},

	"Write":        {client.ToolFile, "file_write", "write"},
	"write_file":   {client.ToolFile, "file_write", "write"},
	"create_file":  {client.ToolFile, "file_write", "write"},
	"Edit":         {client.ToolFile, "file_write", "edit"},
	"edit_file":    {client.ToolFile, "file_write", "edit"},
	"MultiEdit":    {client.ToolFile, "file_write", "edit"},
	"multi_edit":   {client.ToolFile, "file_write", "edit"},
	"NotebookEdit": {client.ToolFile, "file_write", "edit"},
	"apply_patch":  {client.ToolFile, "file_write", "edit"},
	"patch_file":   {client.ToolFile, "file_write", "edit"},

	"Read":         {client.ToolFile, "file_read", "read"},
	"read_file":    {client.ToolFile, "file_read", "read"},
	"NotebookRead": {client.ToolFile, "file_read", "read"},
	"Glob":         {client.ToolFile, "internal", ""},
	"Grep":         {client.ToolFile, "internal", ""},
	"search":       {client.ToolFile, "internal", ""},
	"glob":         {client.ToolFile, "internal", ""},
	"grep":         {client.ToolFile, "internal", ""},

	"WebFetch":  {client.ToolShell, "internal", ""},
	"WebSearch": {client.ToolShell, "internal", ""},
}

func sessionStartMetadata(e *HookEvent) map[string]any {
	return hookflow.SessionStartMetadata(provider, e.Source, sourceValues, e.Model, e.Cwd, e.PermissionMode, permissionModes)
}

const maxIdentLen = 512

// A value outside its set is dropped, never egressed verbatim.
var (
	sourceValues = map[string]bool{"startup": true, "resume": true, "clear": true, "compact": true}
	// reasonValues is SessionEnd's cause; Muse's own values are undocumented.
	reasonValues = map[string]bool{"clear": true, "resume": true, "logout": true, "prompt_input_exit": true, "other": true}
	// compactTriggers is Muse's own vocabulary (soft|hard), passed through as
	// said rather than renamed to Claude Code's manual|auto.
	compactTriggers   = map[string]bool{"soft": true, "hard": true}
	notificationTypes = map[string]bool{
		"permission_prompt": true, "idle_prompt": true, "auth_success": true, "elicitation_dialog": true,
		"elicitation_url_dialog": true, "elicitation_complete": true, "elicitation_response": true,
		"agent_needs_input": true, "agent_completed": true, "quota_auto_resume_fired": true,
		"quota_auto_resume_stale": true, "quota_auto_resume_disabled": true,
	}
	// permissionModes is the closed enum the contract declares for
	// metadata.permission_mode; a Muse mode outside it is dropped.
	permissionModes = map[string]bool{"default": true, "plan": true, "acceptEdits": true, "auto": true, "dontAsk": true, "bypassPermissions": true}
)

// isBumpSource reports whether a SessionStart source opens a new run of the
// same session. Only a resume does: a clear mints a new session id (observed on
// Muse 1.4.1), so it has no prior run under its own id to continue.
func isBumpSource(source string) bool {
	return hookflow.EnumOr(source, sourceValues) == "resume"
}

// capStr delegates so the adapters and the engine cap an identifier the same
// way.
func capStr(s string) string { return hookflow.CapIdent(s) }

func (m Mapper) clock() time.Time { return hookflow.NowOr(m.Now) }

// redact runs a content body through the secret detector before it is attached.
// It lives on the Mapper so every caller of Map inherits it: the gate re-maps
// the same hook event for its own copy, and a call-site redactor would leave
// that copy raw.
func (m Mapper) redact(s string) string { return hookflow.RedactWith(m.RedactContent, s) }

func (m Mapper) gatedToolOutput(text string) *client.Content {
	if out := hookflow.GatedText(m.CaptureContent, m.RedactContent, text); out != "" {
		return &client.Content{ToolOutput: out}
	}
	return nil
}

func (m Mapper) eventID(ev client.DevEvent) string { return hookflow.EventIDOr(m.NewID, deriveID, ev) }

func deriveID(ev client.DevEvent) string {
	return hookflow.DeriveID("mus-", ev, []string{ev.ModelCallRequestID}, nil)
}
