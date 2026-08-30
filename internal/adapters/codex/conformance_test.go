package codex

import (
	"encoding/json"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/conformance"
)

// TestEmittedEventsAreConformant is the cross-contract acceptance check (story
// AC-10): every event the Codex adapter produces must validate against the
// SL-1 dev-event schema with content-capture disabled. If the contract
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

func mustMarshalContractShape(t *testing.T, ev client.DevEvent) []byte {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// TestUsageRollupPairIsConformant closes the seam this adapter's rollup fell
// through: MapUsageRollup does not go through Map, so
// TestEmittedEventsAreConformant above never covered it, and NO turn shape
// from either adapter was ever validated as real mapper output.
func TestUsageRollupPairIsConformant(t *testing.T) {
	m := testMapper()
	in, out, cacheRead := 8102, 1440, 41000
	m.Finops = &FinopsUsage{
		Model:  "gpt-5.3-codex",
		Tokens: &client.Tokens{Input: &in, Output: &out, CacheRead: &cacheRead},
	}

	started, completed, ok := m.MapUsageRollup(&HookEvent{SessionID: "th-1", Reason: "other"})
	if !ok {
		t.Fatal("MapUsageRollup ok=false — the rollup this test exists to validate was not produced")
	}

	for _, tc := range []struct {
		name string
		ev   client.DevEvent
	}{
		{"TurnStarted", started},
		{"TurnCompleted", completed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := mustMarshalContractShape(t, tc.ev)
			if err := conformance.ValidateDevEvent(raw, false); err != nil {
				t.Fatalf("emitted rollup half is not SL-1 conformant:\n%s\nerror: %v", raw, err)
			}
		})
	}
}
