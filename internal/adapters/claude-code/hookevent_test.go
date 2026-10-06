package claudecode

import (
	"strings"
	"testing"
)

func TestParseHookName(t *testing.T) {
	for _, ok := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionEnd"} {
		if _, err := ParseHookName(ok); err != nil {
			t.Errorf("ParseHookName(%q) errored: %v", ok, err)
		}
	}
	if _, err := ParseHookName("Nope"); err == nil {
		t.Error("unknown hook name should error")
	}
}

func TestParseHookEvent(t *testing.T) {
	body := `{"hook_event_name":"PreToolUse","session_id":"s1","cwd":"/r","tool_name":"Edit","tool_input":{"file_path":"a.go"}}`
	ev, err := ParseHookEvent(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.SessionID != "s1" || ev.ToolName != "Edit" || ev.Cwd != "/r" {
		t.Errorf("parsed wrong: %+v", ev)
	}
	if ev.filePath() != "a.go" {
		t.Errorf("filePath = %q, want a.go", ev.filePath())
	}
}

func TestParseHookEvent_Errors(t *testing.T) {
	if _, err := ParseHookEvent(strings.NewReader("")); err == nil {
		t.Error("empty payload should error")
	}
	if _, err := ParseHookEvent(strings.NewReader("{not json")); err == nil {
		t.Error("malformed json should error")
	}
}

func TestFilePath_NotebookAndAbsent(t *testing.T) {
	nb := &HookEvent{ToolInput: []byte(`{"notebook_path":"nb.ipynb"}`)}
	if nb.filePath() != "nb.ipynb" {
		t.Errorf("notebook_path fallback = %q", nb.filePath())
	}
	none := &HookEvent{ToolInput: []byte(`{"command":"ls"}`)}
	if none.filePath() != "" {
		t.Errorf("no path present should yield empty, got %q", none.filePath())
	}
	empty := &HookEvent{}
	if empty.filePath() != "" {
		t.Errorf("nil tool_input should yield empty, got %q", empty.filePath())
	}
}
