package codex

import (
	"encoding/json"
	"fmt"
	"io"
)

// HookName is the Codex hook event this adapter reacts to.
type HookName string

const (
	HookSessionStart     HookName = "SessionStart"
	HookUserPromptSubmit HookName = "UserPromptSubmit"
	HookPreToolUse       HookName = "PreToolUse"
	HookPostToolUse      HookName = "PostToolUse"
	HookSessionEnd       HookName = "SessionEnd"
)

var hookNames = map[HookName]bool{
	HookSessionStart:     true,
	HookUserPromptSubmit: true,
	HookPreToolUse:       true,
	HookPostToolUse:      true,
	HookSessionEnd:       true,
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

	// ToolName preToolUse / PostToolUse.
	ToolName string `json:"tool_name"`
	// ToolUseID pairs a PreToolUse with its PostToolUse (new in 0.145.0, addendum
	// #5); the per-invocation pairing id Claude Code lacks.
	ToolUseID string `json:"tool_use_id"`
	// ToolInput is retained only as an opaque blob for the enforce leg (local,
	// never-egressed decision input). The observe path never decodes it.
	ToolInput json.RawMessage `json:"tool_input"`

	// Reason sessionEnd. The embedded schema pins reason to the single value
	// "other" (not load-bearing here).
	Reason string `json:"reason"`

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
