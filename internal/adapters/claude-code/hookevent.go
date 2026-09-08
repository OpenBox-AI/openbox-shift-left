package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// HookName is the Claude Code hook event this adapter reacts to.
type HookName string

const (
	HookSessionStart     HookName = "SessionStart"
	HookUserPromptSubmit HookName = "UserPromptSubmit"
	HookPreToolUse       HookName = "PreToolUse"
	HookPostToolUse      HookName = "PostToolUse"
	HookSessionEnd       HookName = "SessionEnd"
	// HookStop / HookSubagentStop are the turn boundaries.
	HookStop         HookName = "Stop"
	HookSubagentStop HookName = "SubagentStop"

	// HookPostToolUseFailure is the other half of PostToolUse.
	HookPostToolUseFailure HookName = "PostToolUseFailure"

	// HookSubagentStart is the opening boundary SubagentStop already had a close
	// for.
	HookSubagentStart HookName = "SubagentStart"

	// HookPermissionDenied fires "after auto mode classifier denies a tool call"
	// (2.1.229).
	HookPermissionDenied HookName = "PermissionDenied"

	// HookStopFailure fires when a turn ends in a provider-side error instead of
	// an assistant message; rate limits, billing, auth, overload.
	HookStopFailure HookName = "StopFailure"

	// The 21 constants below round out full Claude Code hook coverage (phase
	// 04's 33-row contract table, rows 12-32), in that table's order.
	// HookWorktreeCreate is deliberately absent: registering it would make this
	// hook responsible for printing the created worktree path back to Claude
	// Code, breaking `claude --worktree`, isolation:"worktree" subagents and
	// background sessions -- a refusal, not an omission.
	HookSetup               HookName = "Setup"
	HookInstructionsLoaded  HookName = "InstructionsLoaded"
	HookUserPromptExpansion HookName = "UserPromptExpansion"
	HookMessageDisplay      HookName = "MessageDisplay"
	HookPermissionRequest   HookName = "PermissionRequest"
	HookPostToolBatch       HookName = "PostToolBatch"
	HookNotification        HookName = "Notification"
	HookTaskCreated         HookName = "TaskCreated"
	HookTaskCompleted       HookName = "TaskCompleted"
	HookTeammateIdle        HookName = "TeammateIdle"
	HookConfigChange        HookName = "ConfigChange"
	HookCwdChanged          HookName = "CwdChanged"
	HookDirectoryAdded      HookName = "DirectoryAdded"
	HookFileChanged         HookName = "FileChanged"
	HookWorktreeRemove      HookName = "WorktreeRemove"
	HookPreCompact          HookName = "PreCompact"
	HookPostCompact         HookName = "PostCompact"
	HookPreModelSwitch      HookName = "PreModelSwitch"
	HookPostModelSwitch     HookName = "PostModelSwitch"
	HookElicitation         HookName = "Elicitation"
	HookElicitationResult   HookName = "ElicitationResult"
)

var hookNames = map[HookName]bool{
	HookSessionStart:        true,
	HookUserPromptSubmit:    true,
	HookPreToolUse:          true,
	HookPostToolUse:         true,
	HookPostToolUseFailure:  true,
	HookSessionEnd:          true,
	HookStop:                true,
	HookSubagentStop:        true,
	HookSubagentStart:       true,
	HookPermissionDenied:    true,
	HookStopFailure:         true,
	HookSetup:               true,
	HookInstructionsLoaded:  true,
	HookUserPromptExpansion: true,
	HookMessageDisplay:      true,
	HookPermissionRequest:   true,
	HookPostToolBatch:       true,
	HookNotification:        true,
	HookTaskCreated:         true,
	HookTaskCompleted:       true,
	HookTeammateIdle:        true,
	HookConfigChange:        true,
	HookCwdChanged:          true,
	HookDirectoryAdded:      true,
	HookFileChanged:         true,
	HookWorktreeRemove:      true,
	HookPreCompact:          true,
	HookPostCompact:         true,
	HookPreModelSwitch:      true,
	HookPostModelSwitch:     true,
	HookElicitation:         true,
	HookElicitationResult:   true,
}

// ParseHookName validates a raw argv value as a known hook name.
func ParseHookName(s string) (HookName, error) {
	h := HookName(s)
	if !hookNames[h] {
		return "", fmt.Errorf("unknown Claude Code hook %q", s)
	}
	return h, nil
}

// HookEvent is the subset of a Claude Code hook's stdin JSON this adapter
// reads.
//
// Deliberately unbound, so it stays incapable of egressing by accident: delta,
// displayContent, tool_calls[].tool_input, tool_calls[].tool_response,
// permission_suggestions (including rules[].ruleContent), requested_schema,
// additionalContext, permissionDecisionReason, watchPaths, decision, and
// ConfigChange's *output* field `reason` (a different thing from the bound
// SessionEnd `reason` below). Binding a field is not egressing it -- but an
// unbound field cannot egress, and that asymmetry is the record.
type HookEvent struct {
	// HookEventName common (present on every hook payload).
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permission_mode"`

	// TranscriptPath is the filesystem path to this session's jsonl transcript.
	// With finops off this field is decoded but never dereferenced.
	TranscriptPath string `json:"transcript_path"`

	// Source is shared by four events with four different enums, resolved
	// per-hook in the mapper (phase 06), never here: SessionStart
	// (startup|resume|clear|compact|fork), DirectoryAdded
	// (slash_command|register_repo_root), ConfigChange
	// (user_settings|project_settings|local_settings|policy_settings|skills)
	// and Pre/PostModelSwitch (command|picker|sdk, plus auto|resume on Post).
	Source string `json:"source"`
	Model  string `json:"model"`

	// ToolName preToolUse / PostToolUse.
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"` // decoded only for a structural file_path

	// ToolUseID pairs a PreToolUse with its PostToolUse. Structural identifier,
	// never content (INV-2).
	ToolUseID string `json:"tool_use_id"`

	// ToolResponse is what the tool produced, on PostToolUse. It is content, and
	// it is bound here by that decision; the change that retires SL3-SEC-3's
	// "tool output never egresses" for the observe path.
	ToolResponse json.RawMessage `json:"tool_response"`

	// AgentID / AgentType identify the subagent an event occurred inside.
	// Structural identifiers, never content (INV-2).
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`

	// Reason is SessionEnd's close reason; a closed enum (reasonValues),
	// allowlisted through enumOr.
	Reason string `json:"reason"`

	// ErrorDetails is StopFailure's free-text elaboration of ErrorType; what the
	// provider said beyond the enum ("retry after 60s").
	ErrorDetails string `json:"error_details"`

	// IsInterrupt separates "the user cancelled this" from "the tool failed" on
	// PostToolUseFailure. PostToolUseFailure's other field, `error`, is free text
	// a tool wrote and is deliberately unbound here; that decision owns it.
	IsInterrupt *bool `json:"is_interrupt"`

	// ErrorType is StopFailure's error class; a closed provider enum, verified
	// against the 2.1.229 schema: authentication_failed, oauth_org_not_allowed,
	// billing_error, rate_limit, overloaded, invalid_request, model_not_found,
	// server_error, max_output_tokens, unknown.
	ErrorType string `json:"error"`

	// LastAssistantMessage is the text of the assistant message that closed this
	// turn (Stop / SubagentStop). Still unbound, deliberately: `error_details`,
	// `background_tasks`, `session_crons`.
	//   - It is decoded here and copied onto an event only under
	LastAssistantMessage string `json:"last_assistant_message"`

	// Prompt is the UserPromptSubmit prompt text; content (INV-2), not
	// structural. It is decoded here but is consumed only by the mapper when
	// content-capture is opted in (Mapper.CaptureContent); with capture off it is
	// never copied onto an emitted event, so it cannot egress by accident.
	Prompt string `json:"prompt"`

	// PromptID is Claude Code's request/response correlator, common to every
	// hook payload once one exists. Absent until the first user input and
	// requires Claude Code v2.1.196 or later, so a success metric reads
	// "present on post-first-prompt events", never "all events" -- it is not on
	// SessionStart, Setup, or a session-start InstructionsLoaded.
	PromptID string `json:"prompt_id"`

	// Trigger is reused by three hooks under two different enums, resolved
	// per-hook in the mapper (phase 06): Setup ∈ {init,maintenance};
	// PreCompact/PostCompact ∈ {manual,auto}.
	Trigger string `json:"trigger"`

	// InstructionsLoaded. Fires many times per session -- volume, not a
	// correctness concern.
	MemoryType      string   `json:"memory_type"`      // User|Project|Local|Managed
	LoadReason      string   `json:"load_reason"`      // session_start|nested_traversal|path_glob_match|include|compact
	TriggerFilePath string   `json:"trigger_file_path"`
	ParentFilePath  string   `json:"parent_file_path"`
	Globs           []string `json:"globs"`

	// FilePath is top-level on InstructionsLoaded, ConfigChange and
	// FileChanged, distinct from tool_input.file_path, which filePath() reads.
	FilePath string `json:"file_path"`

	// UserPromptExpansion. command_args and the pre-expansion `prompt` are
	// deliberately unbound: prompt duplicates prompt_submitted under a second
	// key. command_source is free-form and is capped by capStr in the mapper
	// (R1), not here.
	ExpansionType string `json:"expansion_type"` // slash_command|mcp_prompt
	CommandName   string `json:"command_name"`
	CommandArgs   string `json:"command_args"`
	CommandSource string `json:"command_source"`

	// MessageDisplay. Structural-only (D1): delta/displayContent never bound.
	TurnID    string `json:"turn_id"`
	MessageID string `json:"message_id"`
	Index     *int   `json:"index"`
	Final     *bool  `json:"final"`

	// ToolCalls is PostToolBatch's tool_calls[], read only by
	// batchToolUseIDs(). tool_calls[].tool_input and tool_calls[].tool_response
	// are deliberately not read: tool_calls[] is span-shaped, and no event
	// carries spans[].
	ToolCalls json.RawMessage `json:"tool_calls"`

	// Notification. Message is shared with Elicitation. Title is bound as of
	// v1.9, as gated content in metadata.notification_title -- Message already
	// spends this class's content.signal_detail, and that carrier is one string.
	Message          string `json:"message"`
	Title            string `json:"title"`
	NotificationType string `json:"notification_type"`

	// TaskCreated / TaskCompleted / TeammateIdle. Two signals, never an
	// Activity pair. TaskDescription is bound as of v1.9, as gated content in
	// metadata.task_description -- TaskSubject already spends this class's
	// content.signal_detail. teammate_name and team_name are free-form and are
	// capped by capStr in the mapper (R2), not here; team_name is deprecated.
	TaskID          string `json:"task_id"`
	TaskSubject     string `json:"task_subject"`
	TaskDescription string `json:"task_description"`
	TeammateName    string `json:"teammate_name"`
	TeamName        string `json:"team_name"`

	// CwdChanged. watchPaths' sibling; no decision control either way.
	OldCwd string `json:"old_cwd"`
	NewCwd string `json:"new_cwd"`

	// Directory is DirectoryAdded's target; its `source` reuses the Source
	// field bound above.
	Directory string `json:"directory"`

	// FileChangeEvent is FileChanged's change kind. FileChanged carries
	// file_path (bound above) and this field only -- no body is bound.
	FileChangeEvent string `json:"event"` // change|add|unlink

	// WorktreePath is WorktreeRemove's target. Unpaired by construction: it
	// correlates to WorktreeCreate, which this adapter refuses to register.
	WorktreePath string `json:"worktree_path"`

	// CustomInstructions is PreCompact's content field. The doc types it
	// `string|null`; a JSON null decodes to "" here without error.
	CustomInstructions string `json:"custom_instructions"`

	// CompactSummary is PostCompact's content field.
	CompactSummary string `json:"compact_summary"`

	// Pre/PostModelSwitch. requested_model is `string|null` (null decodes to
	// "" without error). permissionDecisionReason (Pre) and additionalContext
	// (Post) are deliberately unbound; both are output fields.
	FromModel              string   `json:"from_model"`
	ToModel                string   `json:"to_model"`
	RequestedModel         string   `json:"requested_model"`
	CacheTTL               string   `json:"cache_ttl"` // 5m|1h
	Pricing                string   `json:"pricing"`   // configured|catalog|default
	ContextTokens          *int     `json:"context_tokens"`
	PromptCacheWarm        *bool    `json:"prompt_cache_warm"`
	EstimatedCacheWriteUSD *float64 `json:"estimated_cache_write_usd"`

	// Elicitation / ElicitationResult. requested_schema is deliberately
	// unbound (R3): it can embed a caller-supplied schema shape.
	MCPServerName string `json:"mcp_server_name"`
	Mode          string `json:"mode"`   // form|url
	URL           string `json:"url"`
	ElicitationID string `json:"elicitation_id"`
	Action        string `json:"action"` // accept|decline|cancel

	// ElicitationContent is ElicitationResult's submitted form values, read
	// only by elicitationContentText(). Bound as json.RawMessage so it is
	// never accidentally logged by a %v on the struct; it reaches text only
	// through the method the mapper redacts, caps and gates.
	ElicitationContent json.RawMessage `json:"content"`
}

const maxHookPayload = 32 << 20 // 32 MiB

// ParseHookEvent decodes a Claude Code hook payload from r over a bounded
// reader (maxHookPayload).
func ParseHookEvent(r io.Reader) (*HookEvent, error) {
	dec := json.NewDecoder(io.LimitReader(r, maxHookPayload))
	var ev HookEvent
	if err := dec.Decode(&ev); err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("empty hook payload")
		}
		return nil, fmt.Errorf("parse hook payload: %w", err)
	}
	return &ev, nil
}

func (e *HookEvent) filePath() string {
	if len(e.ToolInput) == 0 {
		return ""
	}
	var in struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	}
	if err := json.Unmarshal(e.ToolInput, &in); err != nil {
		return ""
	}
	if in.FilePath != "" {
		return in.FilePath
	}
	return in.NotebookPath
}

// subagentType what is deliberately NOT read from the same tool_input:
// `prompt` and `description`, which are free text the model composed.
func (e *HookEvent) subagentType() string {
	if len(e.ToolInput) == 0 {
		return ""
	}
	var in struct {
		SubagentType string `json:"subagent_type"`
	}
	if err := json.Unmarshal(e.ToolInput, &in); err != nil {
		return ""
	}
	return in.SubagentType
}

// command local-only (INV-2): this is used solely to populate the enforce-mode
// decision.DecisionRequest, which is evaluated in-process on this machine; it
// never egresses to core and is never logged.
func (e *HookEvent) command() string {
	if len(e.ToolInput) == 0 {
		return ""
	}
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(e.ToolInput, &in); err != nil {
		return ""
	}
	return in.Command
}

// fileText it is passed in-process to the decider and is never egressed to
// core and never logged; the observe/telemetry egress path (Mapper) still
// never decodes it, so the metadata-only-on-the-wire posture is unchanged.
func (e *HookEvent) fileText() string {
	if len(e.ToolInput) == 0 {
		return ""
	}
	var in struct {
		Content   string `json:"content"`    // Write
		NewString string `json:"new_string"` // Edit / MultiEdit
	}
	if err := json.Unmarshal(e.ToolInput, &in); err != nil {
		return ""
	}
	if in.Content != "" {
		return in.Content
	}
	return in.NewString
}

func (e *HookEvent) toolOutputText() string {
	raw := bytes.TrimSpace(e.ToolResponse)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	return string(raw)
}

// maxBatchToolUseIDs caps batchToolUseIDs so one PostToolBatch payload cannot
// grow the correlation set without bound.
const maxBatchToolUseIDs = 64

// batchToolUseIDs reads ONLY the tool_use_id of each element of tool_calls[].
// tool_input and tool_response are deliberately not read: tool_calls[] is
// span-shaped, and no event carries spans[]. A malformed array returns nil.
func (e *HookEvent) batchToolUseIDs() []string {
	if len(e.ToolCalls) == 0 {
		return nil
	}
	var calls []struct {
		ToolUseID string `json:"tool_use_id"`
	}
	if err := json.Unmarshal(e.ToolCalls, &calls); err != nil {
		return nil
	}
	ids := make([]string, 0, len(calls))
	for _, c := range calls {
		if c.ToolUseID == "" {
			continue
		}
		ids = append(ids, c.ToolUseID)
		if len(ids) == maxBatchToolUseIDs {
			break
		}
	}
	return ids
}

// elicitationContentText serializes ElicitationResult's submitted form values
// as compact JSON text: "" for empty or null. It is content and is redacted,
// capped and gated in the mapper -- this method only serializes.
func (e *HookEvent) elicitationContentText() string {
	raw := bytes.TrimSpace(e.ElicitationContent)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return ""
	}
	return buf.String()
}
