package codex

import (
	"encoding/json"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/contracts/dev-event/conformance"
)

// TestEmittedEventsAreConformant is the cross-contract acceptance check (story
// AC-10): every event the Codex adapter produces must validate against the
// SL-1 dev-event schema with content-capture DISABLED. If the contract
// tightens, this breaks here rather than silently at ingest.
func TestEmittedEventsAreConformant(t *testing.T) {
	m := testMapper()

	cases := []struct {
		name string
		hook HookName
		ev   *HookEvent
	}{
		{"SessionStart", HookSessionStart, &HookEvent{SessionID: "th-1", Cwd: "/repo", Source: "startup", Model: "gpt-5.3-codex", PermissionMode: "default"}},
		{"UserPromptSubmit", HookUserPromptSubmit, &HookEvent{SessionID: "th-1", PermissionMode: "default", TurnID: "t1"}},
		{"PreToolUse/bash", HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "call-1", ToolInput: json.RawMessage(`{"command":"ls"}`)}},
		{"PreToolUse/apply_patch", HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: "apply_patch", ToolUseID: "call-2"}},
		{"PreToolUse/mcp", HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: "mcp__github__create_issue", ToolUseID: "call-3"}},
		{"PostToolUse/bash", HookPostToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "call-1"}},
		{"PostToolUse/unknown-tool", HookPostToolUse, &HookEvent{SessionID: "th-1", ToolName: "web_search", ToolUseID: "call-4"}},
		{"SessionEnd", HookSessionEnd, &HookEvent{SessionID: "th-1", Reason: "other"}},
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

// mustMarshalContractShape marshals a DevEvent to its on-the-wire contract
// JSON. The client strips content before egress; here we assert the adapter's
// own output (pre-client) is already content-free and conformant.
func mustMarshalContractShape(t *testing.T, ev client.DevEvent) []byte {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
