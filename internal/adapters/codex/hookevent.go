package codex

import (
	"encoding/json"
	"fmt"
	"io"
)

// HookName is the Codex hook event this adapter reacts to.
type HookName string

const (
	HookSessionStart      HookName = "SessionStart"
	HookUserPromptSubmit  HookName = "UserPromptSubmit"
	HookPreToolUse        HookName = "PreToolUse"
	HookPermissionRequest HookName = "PermissionRequest"
	HookPostToolUse       HookName = "PostToolUse"
	HookStop              HookName = "Stop"
	HookSubagentStart     HookName = "SubagentStart"
	HookSubagentStop      HookName = "SubagentStop"
	HookPreCompact        HookName = "PreCompact"
	HookPostCompact       HookName = "PostCompact"
	HookSessionEnd        HookName = "SessionEnd"
)

var hookNames = map[HookName]bool{
	HookSessionStart:      true,
	HookUserPromptSubmit:  true,
	HookPreToolUse:        true,
	HookPermissionRequest: true,
	HookPostToolUse:       true,
	HookStop:              true,
	HookSubagentStart:     true,
	HookSubagentStop:      true,
	HookPreCompact:        true,
	HookPostCompact:       true,
	HookSessionEnd:        true,
}

// ParseHookName validates a raw argv value as a known hook name.
func ParseHookName(s string) (HookName, error) {
	h := HookName(s)
	if !hookNames[h] {
		return "", fmt.Errorf("unknown Codex hook %q", s)
	}
	return h, nil
}

// HookEvent is the subset of a Codex hook's stdin JSON this adapter reads.
// Content), and `tool_response` (PostToolUse) has no field here at all, so
// neither can leak into an emitted event even by accident.
type HookEvent struct {
	// HookEventName common (present on every hook payload).
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permission_mode"` // default|acceptEdits|plan|dontAsk|bypassPermissions
	Model          string `json:"model"`

	// TurnID is Codex's per-turn correlation id ("Codex extension: expose the
	// active turn id").
	TurnID string `json:"turn_id"`

	// TranscriptPath is the filesystem path to this session's rollout transcript
	// (nullable on the wire; decodes to "").
	TranscriptPath string `json:"transcript_path"`

	// Source sessionStart.
	Source string `json:"source"` // startup|resume|clear|compact

	// ToolName preToolUse / PostToolUse / PermissionRequest.
	ToolName string `json:"tool_name"`
	// ToolUseID pairs a PreToolUse with its PostToolUse (new in 0.145.0); the
	// per-invocation pairing id Claude Code lacks. PermissionRequest
	// does NOT carry one -- verified on 0.150.0-alpha.8, where the
	// payload has tool_name and tool_input but no tool_use_id -- so a
	// PermissionRequest cannot be paired to the tool call it escalates from.
	ToolUseID string `json:"tool_use_id"`
	// ToolInput is retained only as an opaque blob for the enforce leg (local,
	// never-egressed decision input). The observe path never decodes it.
	ToolInput json.RawMessage `json:"tool_input"`

	// Reason sessionEnd. The embedded schema pins reason to the single value
	// "other" (not load-bearing here).
	Reason string `json:"reason"`

	// LastAssistantMessage is that turn's final assistant text, required on both
	// Stop and SubagentStop (nullable on the wire, so it decodes to ""). Content
	// (INV-2): consumed only by MapTurn under Mapper.CaptureContent, redacted
	// before attachment.
	LastAssistantMessage string `json:"last_assistant_message"`

	// StopHookActive is Codex's hook-driven-loop breaker. It is READ and never
	// acted on, because this adapter's Stop output is always empty and so no loop
	// is possible. Binding it keeps that deliberate: a later change that wanted to
	// write on Stop would have to confront the field rather than discover it.
	StopHookActive bool `json:"stop_hook_active"`

	// AgentID / AgentType identify a subagent on SubagentStart/SubagentStop; both
	// are required there (verified against the 0.150.0-alpha.8 embedded schema).
	// A sidechain turn without an AgentID is SKIPPED rather than guessed: it would
	// otherwise share the main thread's turn cursor.
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`

	// Trigger is PreCompact/PostCompact's cause: "manual" or "auto".
	Trigger string `json:"trigger"`

	// Prompt is the UserPromptSubmit prompt text; content (INV-2), not
	// structural. It is decoded here but consumed only by the mapper when
	// content-capture is opted in (Mapper.CaptureContent); with capture off it is
	// never copied onto an emitted event (parity with the CC adapter).
	Prompt string `json:"prompt"`
}

// command the observe path never calls it (ToolInput stays an opaque
// json.RawMessage), so observe egress is unchanged.
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

func (e *HookEvent) fileText() string {
	return e.command()
}

const maxHookPayload = 32 << 20 // 32 MiB

// ParseHookEvent decodes a Codex hook payload from r over a bounded reader. A
// malformed or empty body is an error the caller treats fail-open (log to
// stderr, emit nothing, exit 0); never a block (INV-3).
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
