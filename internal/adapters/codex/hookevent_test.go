package codex

import (
	"strings"
	"testing"
)

func TestParseHookName(t *testing.T) {
	for _, ok := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionEnd"} {
		if _, err := ParseHookName(ok); err != nil {
			t.Errorf("ParseHookName(%q): %v", ok, err)
		}
	}
	// The six unwired 0.145.0 events are recognized-invalid here (non-goals).
	for _, bad := range []string{"Stop", "SubagentStart", "SubagentStop", "PermissionRequest", "PreCompact", "PostCompact", "flush", ""} {
		if _, err := ParseHookName(bad); err == nil {
			t.Errorf("ParseHookName(%q) should be rejected", bad)
		}
	}
}

// TestParseHookEvent_RealShapedPayload decodes a payload with every field the
// 0.145.0 pre-tool-use.command.input schema requires (incl. a null
// transcript_path and fields we deliberately do NOT bind, like tool_response).
func TestParseHookEvent_RealShapedPayload(t *testing.T) {
	payload := `{
		"hook_event_name": "PostToolUse",
		"session_id": "0195c7e4-1111-7000-8000-000000000001",
		"turn_id": "turn-3",
		"cwd": "/work/repo",
		"transcript_path": null,
		"model": "gpt-5.3-codex",
		"permission_mode": "acceptEdits",
		"tool_name": "Bash",
		"tool_use_id": "call-xyz",
		"tool_input": {"command": "ls -la"},
		"tool_response": {"output": "SHOULD-NOT-BE-BOUND"}
	}`
	ev, err := ParseHookEvent(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.SessionID != "0195c7e4-1111-7000-8000-000000000001" || ev.TurnID != "turn-3" ||
		ev.ToolName != "Bash" || ev.ToolUseID != "call-xyz" || ev.PermissionMode != "acceptEdits" {
		t.Errorf("structural fields wrong: %+v", ev)
	}
	if ev.TranscriptPath != "" {
		t.Errorf("null transcript_path should decode to empty, got %q", ev.TranscriptPath)
	}
	// tool_response has no field on HookEvent by construction (SL3-SEC-3).
}

func TestParseHookEvent_EmptyAndMalformed(t *testing.T) {
	if _, err := ParseHookEvent(strings.NewReader("")); err == nil {
		t.Error("empty payload should error (handled fail-open by the caller)")
	}
	if _, err := ParseHookEvent(strings.NewReader("{not json")); err == nil {
		t.Error("malformed payload should error")
	}
}
