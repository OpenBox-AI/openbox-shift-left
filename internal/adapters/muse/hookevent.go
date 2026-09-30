package muse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// HookName is the Muse hook event this adapter reacts to.
type HookName string

const (
	HookSessionStart       HookName = "SessionStart"
	HookUserPromptSubmit   HookName = "UserPromptSubmit"
	HookPreToolUse         HookName = "PreToolUse"
	HookPermissionRequest  HookName = "PermissionRequest"
	HookPostToolUse        HookName = "PostToolUse"
	HookPostToolUseFailure HookName = "PostToolUseFailure"
	HookSubagentStart      HookName = "SubagentStart"
	HookSubagentStop       HookName = "SubagentStop"
	HookStop               HookName = "Stop"
	HookStopFailure        HookName = "StopFailure"
	HookSessionEnd         HookName = "SessionEnd"
	HookPreLLMCall         HookName = "PreLLMCall"
	HookPostLLMCall        HookName = "PostLLMCall"

	// The five Muse events with no contract type. A handler for one of these is
	// never installed; one that runs anyway is a no-op.
	HookPreCompact    HookName = "PreCompact"
	HookPostCompact   HookName = "PostCompact"
	HookNotification  HookName = "Notification"
	HookPostToolBatch HookName = "PostToolBatch"
	HookInterrupt     HookName = "Interrupt"
)

var hookNames = map[HookName]bool{
	HookSessionStart: true, HookUserPromptSubmit: true, HookPreToolUse: true,
	HookPermissionRequest: true, HookPostToolUse: true, HookPostToolUseFailure: true,
	HookSubagentStart: true, HookSubagentStop: true, HookStop: true, HookStopFailure: true,
	HookSessionEnd: true, HookPreLLMCall: true, HookPostLLMCall: true,
	HookPreCompact: true, HookPostCompact: true, HookNotification: true,
	HookPostToolBatch: true, HookInterrupt: true,
}

// ParseHookName validates a raw argv value as a known hook name.
func ParseHookName(s string) (HookName, error) {
	h := HookName(s)
	if !hookNames[h] {
		return "", fmt.Errorf("unknown Muse hook %q", s)
	}
	return h, nil
}

// Gated reports whether the hook can deny, so its answer (or the exit code of
// a crash) is what stands between Muse and an ungoverned call. It is the one
// source of truth for the gate branch, the fault exit code and the installer's
// raised ceiling.
func (h HookName) Gated() bool {
	switch h {
	case HookUserPromptSubmit, HookPreToolUse, HookPermissionRequest, HookPreLLMCall:
		return true
	}
	return false
}

// Observed reports whether the hook produces anything at all. The rest have no
// contract type: a turn's usage is never taken from a hook payload, and a
// completion is never fabricated. Stop reports nothing either, but it is where
// the session-log reconciler runs, so it is installed. SubagentStop is
// observed: a subagent is a session of its own, and this is its SessionEnded.
func (h HookName) Observed() bool {
	switch h {
	case HookPreCompact, HookPostCompact,
		HookNotification, HookPostToolBatch, HookInterrupt:
		return false
	}
	return hookNames[h]
}

// HookEvent is the subset of a Muse hook's stdin JSON this adapter reads.
//
// Decoding is deliberately tolerant: every field is read by key from a generic
// object, a value of an unexpected type reads as empty rather than failing the
// payload, and unknown keys are ignored. The payload shapes are documented but
// not verified on a binary, and a decode failure on a gated event is a denial,
// so a type surprise in a field nothing depends on must not become one.
//
// Content (INV-2): tool_response, the prompt and the message previews are
// decoded but reach an event only through the Mapper's capture gate and
// redactor. usage and options are read for the local trace only.
type HookEvent struct {
	HookEventName  string
	SessionID      string
	TurnID         string
	Cwd            string
	TranscriptPath string
	Model          string
	PermissionMode string
	ModelProvider  string

	// Source is SessionStart's cause: startup, resume or clear.
	Source string
	// Reason is SessionEnd's cause.
	Reason string
	// Prompt is UserPromptSubmit's text; content, never copied without capture.
	Prompt string

	ToolName string
	// ToolUseID pairs a PreToolUse with its PostToolUse. PermissionRequest
	// carries none.
	ToolUseID    string
	ToolInput    json.RawMessage
	ToolResponse json.RawMessage
	// Error is PostToolUseFailure's or PostLLMCall's error, a string or an
	// object; only a structural class is ever taken from it.
	Error json.RawMessage

	// SubagentID and ChildSessionID identify a subagent on SubagentStart.
	SubagentID     string
	ChildSessionID string

	// Model-call keys, on PreLLMCall and repeated on PostLLMCall.
	Provider  string
	RequestID string
	Attempt   json.RawMessage
	Step      json.RawMessage
	Messages  []json.RawMessage
	Tools     []json.RawMessage
	// MessageCount and ToolCount are read as numbers and fall back to the
	// length of the list beside them.
	MessageCount json.RawMessage
	ToolCount    json.RawMessage
	Options      json.RawMessage

	// PostLLMCall-only keys.
	Status        string
	ResponseID    string
	FinishReason  string
	ToolCallCount json.RawMessage
	Usage         json.RawMessage
}

// UnmarshalJSON binds the fields above from a generic object.
func (e *HookEvent) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("hook payload is not an object")
	}
	list := func(k string) []json.RawMessage {
		var out []json.RawMessage
		if err := json.Unmarshal(m[k], &out); err != nil {
			return nil
		}
		return out
	}
	*e = HookEvent{
		HookEventName:  str(m["hook_event_name"]),
		SessionID:      str(m["session_id"]),
		TurnID:         str(m["turn_id"]),
		Cwd:            str(m["cwd"]),
		TranscriptPath: str(m["transcript_path"]),
		Model:          str(m["model"]),
		PermissionMode: str(m["permission_mode"]),
		ModelProvider:  str(m["model_provider"]),
		Source:         str(m["source"]),
		Reason:         str(m["reason"]),
		Prompt:         str(m["prompt"]),
		ToolName:       str(m["tool_name"]),
		ToolUseID:      str(m["tool_use_id"]),
		ToolInput:      m["tool_input"],
		ToolResponse:   m["tool_response"],
		SubagentID:     str(m["subagent_id"]),
		ChildSessionID: str(m["child_session_id"]),
		Provider:       str(m["provider"]),
		RequestID:      str(m["request_id"]),
		Attempt:        m["attempt"],
		Step:           m["step"],
		Messages:       list("messages"),
		Tools:          list("tools"),
		MessageCount:   m["message_count"],
		ToolCount:      m["tool_count"],
		Options:        m["options"],
		Status:         str(m["status"]),
		ResponseID:     str(m["response_id"]),
		FinishReason:   str(m["finish_reason"]),
		ToolCallCount:  m["tool_call_count"],
		Usage:          m["usage"],
		Error:          m["error"],
	}
	return nil
}

// str reads a JSON string, or the literal of a number or bool; anything else,
// including null, is "".
func str(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	case '{', '[', 'n':
		return ""
	}
	return string(raw)
}

// intOf reads a JSON number, or a string holding one, as a non-negative int.
func intOf(raw json.RawMessage) (int, bool) {
	s := strings.TrimSpace(str(raw))
	if s == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 {
			return 0, false
		}
		return n, true
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f >= 0 && f == float64(int(f)) {
		return int(f), true
	}
	return 0, false
}

// field reads one string key from a tool_input object.
func (e *HookEvent) field(key string) string {
	if len(e.ToolInput) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(e.ToolInput, &m); err != nil {
		return ""
	}
	raw := bytes.TrimSpace(m[key])
	if len(raw) == 0 || raw[0] != '"' {
		return ""
	}
	return str(raw)
}

// command is the shell command of a Bash-class call. It feeds only the local
// decider and the gated tool-input content; observe metadata never carries it.
func (e *HookEvent) command() string { return e.field("command") }

// shellText is what a shell-class call runs: its command, or, for a shell tool
// whose input is not shaped {command} (bash_input), the whole tool_input, so a
// payload under an unexpected key still reaches the decider and the content
// gate instead of reading as empty.
func (e *HookEvent) shellText() string {
	if c := e.command(); c != "" {
		return c
	}
	raw := strings.TrimSpace(string(e.ToolInput))
	if raw == "null" {
		return ""
	}
	return raw
}

// filePath is a file tool's target, a structural locator.
func (e *HookEvent) filePath() string {
	for _, k := range filePathKeys {
		if p := e.field(k); p != "" {
			return p
		}
	}
	return ""
}

// fileText is the body a write or edit tool puts on disk, read at the first of
// contentFieldKeys that holds one; the same precedence RedactToolInput
// rewrites at, so the decider and the rewrite agree on which field is the body.
func (e *HookEvent) fileText() string {
	for _, k := range contentFieldKeys {
		if s := e.field(k); s != "" {
			return s
		}
	}
	return ""
}

// outputText is a tool_response or error as text: an object's `output` (or
// `error`) string, else the raw JSON. Muse's bash tool delivers its result as a
// string that itself holds a JSON object ({"output": ..., "exit_code": ...});
// that string is unwrapped the same way, and a string that is not such an
// object is taken as is.
func outputText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		s := str(raw)
		if inner := strings.TrimSpace(s); strings.HasPrefix(inner, "{") {
			if t := outputText(json.RawMessage(inner)); t != inner && t != "" {
				return t
			}
		}
		return s
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err == nil {
		for _, k := range []string{"output", "error", "stdout"} {
			if s := str(m[k]); s != "" {
				return s
			}
		}
	}
	return string(raw)
}

const maxHookPayload = 32 << 20 // 32 MiB; Muse itself never delivers more than 256 KiB

// ParseHookEvent decodes a Muse hook payload from r over a bounded reader.
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
