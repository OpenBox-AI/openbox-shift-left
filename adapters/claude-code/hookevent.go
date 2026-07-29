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

// HookEvent is the subset of a Claude Code hook's stdin JSON this adapter
// reads.
//
// It captures only non-content, structural fields (INV-2) — session id,
// working directory, tool identity, lifecycle enums — with one deliberate,
// gated exception: Prompt (the UserPromptSubmit text). Command strings,
// file contents, and tool output remain intentionally not decoded, so they
// cannot leak into an emitted event even by accident. Prompt is decoded but
// is copied onto an event only under the content-capture opt-in
// (Mapper.CaptureContent) — with capture off it is inert, exactly like the
// structural-only fields. Unknown/extra fields are ignored
// (forward-compatible with Claude Code drift).
type HookEvent struct {
	// Common (present on every hook payload).
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permission_mode"`

	// TranscriptPath is the filesystem path to this session's JSONL
	// transcript. It is a structural locator (like Cwd), not content —
	// INV-2 permits it. The file it points at is content-bearing, so it is
	// opened only on SessionEnd and only when the opt-in finops gate is set
	// (ResolveFinops), and even then only a projection-only parser touches
	// it, extracting usage numbers only. With finops off this field is
	// decoded but never dereferenced.
	TranscriptPath string `json:"transcript_path"`

	// SessionStart.
	Source string `json:"source"` // startup|resume|clear|compact
	Model  string `json:"model"`

	// PreToolUse / PostToolUse.
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"` // decoded only for a structural file_path

	// ToolUseID pairs a PreToolUse with its PostToolUse. Claude Code carries
	// it on both, so it replaces the heuristic (session, tool, locator)
	// pairing for non-MCP tools — two identical sequential Bash calls no
	// longer collide onto one span (see mapper.mapTool). Structural
	// identifier, never content (INV-2). Absent on older Claude Code
	// versions, which fall back to the heuristic derivation.
	ToolUseID string `json:"tool_use_id"`

	// AgentID / AgentType identify the subagent an event occurred inside.
	// They ride *every* hook payload fired within a subagent (not only the
	// Subagent* hooks), so a session's subagent tree is reconstructable from
	// the tool events alone: events sharing an AgentID belong to one subagent
	// and AgentType names its kind. Empty on the main agent's own events.
	// Structural identifiers, never content (INV-2).
	//
	// Verified against the installed claude 2.1.220 binary, which documents
	// agent_id as: "Subagent identifier. Present only when the hook fires
	// from within a subagent (e.g., a tool called by an AgentTool worker).
	// Absent for the main thread, even in --agent sessions. Use this field
	// (not agent_type) to distinguish subagent calls from main-thread calls."
	// Note parent_agent_id is deliberately absent here: it exists only as an
	// OTel span attribute on claude_code.subagent.spawn, not as a hook
	// payload field, so the tree is flat-by-agent_id rather than parented.
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`

	// SessionEnd.
	Reason string `json:"reason"`

	// Prompt is the UserPromptSubmit prompt text — content (INV-2), not
	// structural. It is decoded here but is consumed only by the mapper
	// when content-capture is opted in (Mapper.CaptureContent); with
	// capture off it is never copied onto an emitted event, so it cannot
	// egress by accident. Redaction at source is a separate layer
	// ([EXT-guardrail-redaction]).
	Prompt string `json:"prompt"`
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

// command extracts the shell command string from a Bash tool_input.
//
// Local-only (INV-2): this is used solely to populate the enforce-mode
// decision.DecisionRequest, which is evaluated in-process on this machine —
// it never egresses to core and is never logged. It is the axis a local
// policy matches a dangerous command on (the canonical `rm -rf …` rule),
// analogous to the SDK sending activity_input to its own governance gate.
// The observe/telemetry egress path (Mapper) still never decodes the
// command, so the metadata-only-on-the-wire posture is unchanged; the
// command is read here only for the never-egressed local decision. Returns
// "" for a non-Bash tool or an absent/unparsable command.
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

// fileText extracts the file body a file-write tool carries — Claude
// Code's Write uses "content", Edit uses "new_string". This is content,
// not a structural locator. (MultiEdit nests bodies under
// edits[].new_string; extracting those is deferred — under-capturing is
// the INV-2-safe direction, since a missing body just means nothing local
// to redact.)
//
// Local-only (INV-2), and stricter than command(): it is read solely to
// populate the enforce-mode decision.DecisionRequest.Content when the org
// opted into content capture (ResolveContentCapture), so a
// redaction-capable local evaluator has the body to redact — the analog of
// the reference SDK sending the full activity_input to its gate. It is
// passed in-process to the decider and is never egressed to core and never
// logged; the observe/telemetry egress path (Mapper) still never decodes
// it, so the metadata-only-on-the-wire posture is unchanged. Returns "" for
// a non-file tool, an absent/unparsable body, or (via the caller's gate)
// when content capture is off.
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
