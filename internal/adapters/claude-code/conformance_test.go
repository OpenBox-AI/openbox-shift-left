package claudecode

import (
	"encoding/json"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/conformance"
)

// TestEmittedEventsAreConformant is the cross-contract acceptance check: every
// event the Claude Code adapter produces must validate against the SL-1 dev-
// event schema with content-capture disabled (the default).
func TestEmittedEventsAreConformant(t *testing.T) {
	m := testMapper()

	cases := []struct {
		name string
		hook HookName
		ev   *HookEvent
	}{
		{"SessionStart", HookSessionStart, &HookEvent{SessionID: "s1", Cwd: "/repo", Source: "startup", Model: "claude-opus-4-8"}},
		{"UserPromptSubmit", HookUserPromptSubmit, &HookEvent{SessionID: "s1", PermissionMode: "default"}},
		{"PreToolUse/file", HookPreToolUse, &HookEvent{SessionID: "s1", ToolName: "Edit", ToolInput: json.RawMessage(`{"file_path":"a.go"}`)}},
		{"PreToolUse/bash", HookPreToolUse, &HookEvent{SessionID: "s1", ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"ls"}`)}},
		{"PreToolUse/mcp", HookPreToolUse, &HookEvent{SessionID: "s1", ToolName: "mcp__github__create_issue"}},
		{"PostToolUse/file", HookPostToolUse, &HookEvent{SessionID: "s1", ToolName: "Read", ToolInput: json.RawMessage(`{"file_path":"a.go"}`)}},
		{"SessionEnd", HookSessionEnd, &HookEvent{SessionID: "s1", Reason: "logout"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := m.Map(tc.hook, tc.ev)
			if !ok {
				t.Fatalf("Map ok=false")
			}
			raw := mustMarshalContractShape(t, ev)
			if err := conformance.ValidateDevEvent(raw, false); err != nil {
				t.Fatalf("emitted event is not SL-1 conformant:\n%s\nerror: %v", raw, err)
			}
		})
	}
}

func mustMarshalContractShape(t *testing.T, ev client.DevEvent) []byte {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
