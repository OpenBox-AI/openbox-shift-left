package claudecode

import (
	"encoding/json"
	"fmt"
	"io"
)

// HookName is the Claude Code hook event this adapter reacts to. It is passed to
// the hook binary as an argv subcommand (deterministic, independent of the
// payload) and cross-checked against the payload's hook_event_name when present.
type HookName string

const (
	HookSessionStart     HookName = "SessionStart"
	HookUserPromptSubmit HookName = "UserPromptSubmit"
	HookPreToolUse       HookName = "PreToolUse"
	HookPostToolUse      HookName = "PostToolUse"
	HookSessionEnd       HookName = "SessionEnd"
)

// hookNames is the set the plugin wires (capabilities.go / plugin/hooks.json).
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
		return "", fmt.Errorf("unknown Claude Code hook %q", s)
	}
	return h, nil
}

// HookEvent is the subset of a Claude Code hook's stdin JSON this adapter reads.
//
// It deliberately captures ONLY non-content, structural fields (INV-2): the
// session id, the working directory, the tool identity, and lifecycle enums.
// Content-bearing fields — the prompt text, a Bash command string, file
// contents, tool output — are intentionally NOT decoded here, so they cannot
// leak into an emitted event even by accident (SL3-SEC-3). Unknown/extra fields
// are ignored (forward-compatible with Claude Code schema drift).
type HookEvent struct {
	// Common (present on every hook payload).
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permission_mode"`

	// TranscriptPath is the filesystem path to this session's JSONL transcript.
	// It is a structural LOCATOR (like Cwd), not content — INV-2 permits it. The
	// file it points at IS content-bearing, so it is opened ONLY on SessionEnd and
	// ONLY when the opt-in finops gate is set (ResolveFinops), and even then only a
	// projection-only parser touches it, extracting usage NUMBERS ONLY (STORY-SL-16
	// / OD-FINOPS). With finops off this field is decoded but never dereferenced.
	TranscriptPath string `json:"transcript_path"`

	// SessionStart.
	Source string `json:"source"` // startup|resume|clear|compact
	Model  string `json:"model"`

	// PreToolUse / PostToolUse.
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"` // decoded only for a structural file_path

	// SessionEnd.
	Reason string `json:"reason"`
}

// maxHookPayload bounds the stdin read so a pathological payload can't exhaust
// memory. It is generous (a PreToolUse payload for a large file Write carries
// the whole file_text) so real large-write events are not dropped — only the
// structural fields are kept regardless of size. Beyond this, decode fails and
// the event is dropped fail-open.
const maxHookPayload = 32 << 20 // 32 MiB

// ParseHookEvent decodes a Claude Code hook payload from r over a bounded reader
// (maxHookPayload). Only structural fields are bound to HookEvent; prompt text,
// command strings, file contents, and tool output have no field here, so the
// decoder scans past them without capturing them. (tool_input is kept as raw
// bytes but is read only for a structural file_path and is never egressed.)
// A malformed or empty body is an error the caller treats fail-open (log to
// stderr, emit nothing, exit 0) — never a block (INV-3).
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

// filePath extracts a structural file path from tool_input for a file tool.
// Claude Code's file tools use "file_path" (Read/Write/Edit/MultiEdit) or
// "notebook_path" (notebook tools). It returns "" when neither is present. The
// path is a structural locator (INV-2 permits it — it is not file content).
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
