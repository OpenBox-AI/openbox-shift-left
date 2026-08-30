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
)

var hookNames = map[HookName]bool{
	HookSessionStart:       true,
	HookUserPromptSubmit:   true,
	HookPreToolUse:         true,
	HookPostToolUse:        true,
	HookPostToolUseFailure: true,
	HookSessionEnd:         true,
	HookStop:               true,
	HookSubagentStop:       true,
	HookSubagentStart:      true,
	HookPermissionDenied:   true,
	HookStopFailure:        true,
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
type HookEvent struct {
	// HookEventName common (present on every hook payload).
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permission_mode"`

	// TranscriptPath is the filesystem path to this session's jsonl transcript.
	// With finops off this field is decoded but never dereferenced.
	TranscriptPath string `json:"transcript_path"`

	// Source sessionStart.
	Source string `json:"source"` // startup|resume|clear|compact
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
