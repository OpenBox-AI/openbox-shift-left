package codex

import (
	"encoding/json"
	"fmt"
	"io"
)

// HookName is the Codex hook event this adapter reacts to. It is passed to the
// hook binary as an argv subcommand (deterministic, independent of the payload)
// — the installer bakes `hook codex <event>` into each hooks.json entry.
//
// Codex v0.145.0 exposes 11 events (spike S5 2026-07-23 addendum); this adapter
// wires the five below — the exact Claude Code parity set (STORY-SL7-A).
// PermissionRequest/Subagent*/`*Compact`/Stop are explicit non-goals.
type HookName string

const (
	HookSessionStart     HookName = "SessionStart"
	HookUserPromptSubmit HookName = "UserPromptSubmit"
	HookPreToolUse       HookName = "PreToolUse"
	HookPostToolUse      HookName = "PostToolUse"
	HookSessionEnd       HookName = "SessionEnd"
)

// hookNames is the set the installer wires (capabilities.go / installer.go).
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
// Field names are grounded in the per-event JSON Schemas embedded in the
// codex-cli 0.145.0 binary (titles `<event>.command.input`) and
// `codex-rs/hooks/src/events/*.rs` @ tag rust-v0.145.0, which confirm the
// spike S5 2026-07-23 addendum. Codex session ≡ thread: `session_id` is the
// OpenBox session id.
//
// It captures ONLY non-content, structural fields (INV-2) — session/turn/tool
// identifiers, working directory, lifecycle enums — with ONE deliberate, gated
// exception: Prompt (the UserPromptSubmit text). `tool_input` is kept as raw
// bytes but is NEVER decoded on the observe path (unlike Claude Code, Codex
// tool_input carries no structural file_path — apply_patch's input is the patch
// body itself, i.e. content), and `tool_response` (PostToolUse) has no field
// here at all, so neither can leak into an emitted event even by accident
// (SL3-SEC-3). Unknown/extra fields are ignored (forward-compatible).
type HookEvent struct {
	// Common (present on every hook payload).
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permission_mode"` // default|acceptEdits|plan|dontAsk|bypassPermissions
	Model          string `json:"model"`

	// TurnID is Codex's per-turn correlation id ("Codex extension: expose the
	// active turn id"). Present on every wired event except SessionStart and
	// SessionEnd (per the embedded schemas). Structural identifier, not content.
	TurnID string `json:"turn_id"`

	// TranscriptPath is the filesystem path to this session's rollout transcript
	// (nullable on the wire; decodes to ""). A structural LOCATOR, not content.
	// This adapter NEVER dereferences it — finops usage extraction from the
	// rollout JSONL is an explicit SL7-A non-goal (the SessionEnd transcript
	// flush makes it a clean follow-up).
	TranscriptPath string `json:"transcript_path"`

	// SessionStart.
	Source string `json:"source"` // startup|resume|clear|compact

	// PreToolUse / PostToolUse.
	ToolName string `json:"tool_name"`
	// ToolUseID pairs a PreToolUse with its PostToolUse (new in 0.145.0,
	// addendum #5) — the per-invocation pairing id Claude Code lacks. Structural.
	ToolUseID string `json:"tool_use_id"`
	// ToolInput is retained ONLY as an opaque blob for the SL7-B enforce leg
	// (local, never-egressed decision input). The observe path never decodes it.
	ToolInput json.RawMessage `json:"tool_input"`

	// SessionEnd. The embedded schema pins reason to the single value "other"
	// (a delta from the addendum's field list, which omitted it — recorded in
	// the story findings; not load-bearing).
	Reason string `json:"reason"`

	// Prompt is the UserPromptSubmit prompt text — CONTENT (INV-2), not
	// structural. It is decoded here but consumed ONLY by the mapper when
	// content-capture is opted in (Mapper.CaptureContent); with capture off it
	// is never copied onto an emitted event (E7-S7 / OD4 parity with the CC
	// adapter).
	Prompt string `json:"prompt"`
}

// command decodes the shell command / patch body from tool_input["command"].
//
// ENFORCE-ONLY (STORY-SL7-B): this is the ONE place tool_input is decoded, and it
// runs ONLY on the enforce PreToolUse path. The observe path NEVER calls it
// (ToolInput stays an opaque json.RawMessage — SL3-SEC-3), so observe egress is
// unchanged. The decoded value is LOCAL-ONLY: it feeds the in-process decider's
// match axis / redaction and is never egressed or logged (INV-2).
//
// Codex's PreToolUse tool_input is {"command":<string>} for BOTH the shell tool
// (Bash — the command) and the file tool (apply_patch — the raw patch text), and
// updatedInput rewrites are re-parsed via updated_input["command"] (grounded @
// rust-v0.145.0 core registry ApplyPatchHandler.pre_tool_use_payload +
// handlers/mod.rs updated_hook_command). A malformed/absent field yields "".
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

// fileText is the redactable BODY for the file class (apply_patch), which Codex
// carries in tool_input["command"] — the raw patch text (delta from Claude Code's
// content/new_string). ENFORCE-ONLY and LOCAL-ONLY (INV-2), like command().
func (e *HookEvent) fileText() string {
	return e.command()
}

// maxHookPayload bounds the stdin read so a pathological payload can't exhaust
// memory. Generous (a PreToolUse payload for a large apply_patch carries the
// whole patch text) so real large events are not dropped — only structural
// fields are kept regardless of size. Beyond this, decode fails and the event
// is dropped fail-open.
const maxHookPayload = 32 << 20 // 32 MiB

// ParseHookEvent decodes a Codex hook payload from r over a bounded reader.
// Only structural fields (plus the capture-gated prompt) are bound to
// HookEvent; command strings, patch bodies, and tool output have no decoded
// field here, so the decoder scans past them without capturing them. A
// malformed or empty body is an error the caller treats fail-open (log to
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
