package codex

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

const testDID = "did:aip:7f3c9b2e-0000-5000-a000-000000000001"

func testMapper() Mapper {
	m := NewMapper(Identity{DeveloperDID: testDID})
	m.Now = func() time.Time { return time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC) }
	m.NewID = func() string { return "evt-fixed" }
	return m
}

// TestClassifyTool_GroundedLiterals is the durable record of the Codex hook
// tool_name literals (story AC-4: "ground the exact tool_name literals ... And
// record them in a test").
func TestClassifyTool_GroundedLiterals(t *testing.T) {
	tests := []struct {
		name string
		kind client.ToolKind
		sem  string
	}{
		{"Bash", client.ToolShell, "internal"},
		{"apply_patch", client.ToolFile, "file_write"},
		{"mcp__github__create_issue", client.ToolMCP, "mcp_tool_call"},
		{"web_search", client.ToolShell, "internal"},
		{"update_plan", client.ToolShell, "internal"},
		{"view_image", client.ToolShell, "internal"},
		{"spawn_agent", client.ToolShell, "internal"},
		{"Write", client.ToolShell, "internal"},
		{"Edit", client.ToolShell, "internal"},
	}
	for _, tt := range tests {
		kind, sem, _, _, _ := classifyTool(tt.name)
		if kind != tt.kind || sem != tt.sem {
			t.Errorf("classifyTool(%q) = (%q,%q), want (%q,%q)", tt.name, kind, sem, tt.kind, tt.sem)
		}
	}
}

func TestMap_LifecycleAndToolEvents(t *testing.T) {
	m := testMapper()

	tests := []struct {
		name     string
		hook     HookName
		ev       *HookEvent
		wantType client.EventType
		wantTool client.Tool
		wantSpan *client.Span // nil = expect no span
	}{
		{
			name:     "session start",
			hook:     HookSessionStart,
			ev:       &HookEvent{SessionID: "th-1", Cwd: "/repo", Source: "startup", Model: "gpt-5.3-codex", PermissionMode: "default"},
			wantType: client.EventSessionStarted,
			wantTool: client.Tool{Name: "codex", Kind: client.ToolShell},
		},
		{
			name:     "prompt submitted",
			hook:     HookUserPromptSubmit,
			ev:       &HookEvent{SessionID: "th-1", PermissionMode: "default", TurnID: "turn-1"},
			wantType: client.EventPromptSubmitted,
			wantTool: client.Tool{Name: "codex", Kind: client.ToolShell},
		},
		{
			name:     "pretooluse bash → shell internal, invocation + command operation id",
			hook:     HookPreToolUse,
			ev:       &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "call-abc", ToolInput: json.RawMessage(`{"command":"ls"}`)},
			wantType: client.EventToolCall,
			wantTool: client.Tool{Name: "Bash", Kind: client.ToolShell},
			wantSpan: &client.Span{SemanticType: "internal", Stage: "started", InvocationID: "call-abc", OperationID: client.OperationForCommand("ls")},
		},
		{
			name:     "posttooluse apply_patch → file_write completed",
			hook:     HookPostToolUse,
			ev:       &HookEvent{SessionID: "th-1", ToolName: "apply_patch", ToolUseID: "call-def"},
			wantType: client.EventToolResult,
			wantTool: client.Tool{Name: "apply_patch", Kind: client.ToolFile},
			wantSpan: &client.Span{SemanticType: "file_write", Stage: "completed", FileOp: "edit", InvocationID: "call-def", OperationID: "call-def"},
		},
		{
			name:     "pretooluse mcp → mcp_tool_call (function stays the MCP tool)",
			hook:     HookPreToolUse,
			ev:       &HookEvent{SessionID: "th-1", ToolName: "mcp__github__create_issue", ToolUseID: "call-ghi"},
			wantType: client.EventToolCall,
			wantTool: client.Tool{Name: "mcp__github__create_issue", Kind: client.ToolMCP, MCPServer: "github"},
			wantSpan: &client.Span{SemanticType: "mcp_tool_call", Stage: "started", MCPServer: "github", Function: "create_issue", InvocationID: "call-ghi"},
		},
		{
			name:     "unknown tool → shell internal catch-all",
			hook:     HookPreToolUse,
			ev:       &HookEvent{SessionID: "th-1", ToolName: "web_search", ToolUseID: "call-jkl"},
			wantType: client.EventToolCall,
			wantTool: client.Tool{Name: "web_search", Kind: client.ToolShell},
			wantSpan: &client.Span{SemanticType: "internal", Stage: "started", InvocationID: "call-jkl", OperationID: "call-jkl"},
		},
		{
			name:     "session end",
			hook:     HookSessionEnd,
			ev:       &HookEvent{SessionID: "th-1", Reason: "other"},
			wantType: client.EventSessionEnded,
			wantTool: client.Tool{Name: "codex", Kind: client.ToolShell},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := m.Map(tt.hook, tt.ev)
			if !ok {
				t.Fatalf("Map returned ok=false, want an event")
			}
			if got.EventType != tt.wantType {
				t.Errorf("event_type = %q, want %q", got.EventType, tt.wantType)
			}
			if got.Tool != tt.wantTool {
				t.Errorf("tool = %+v, want %+v", got.Tool, tt.wantTool)
			}
			if got.SessionID != tt.ev.SessionID {
				t.Errorf("session id = %q, want %q (session ≡ thread)", got.SessionID, tt.ev.SessionID)
			}
			if got.DeveloperDID != testDID {
				t.Errorf("developer_did = %q, want %q", got.DeveloperDID, testDID)
			}
			if got.EventID == "" {
				t.Error("event_id is empty (INV-5)")
			}
			if got.Content != nil {
				t.Errorf("content must be nil, got %+v", got.Content)
			}
			switch {
			case tt.wantSpan == nil && got.Span != nil:
				t.Errorf("expected no span, got %+v", got.Span)
			case tt.wantSpan != nil && got.Span == nil:
				t.Errorf("expected span %+v, got nil", tt.wantSpan)
			case tt.wantSpan != nil:
				if !reflect.DeepEqual(*got.Span, *tt.wantSpan) {
					t.Errorf("span = %+v, want %+v", *got.Span, *tt.wantSpan)
				}
			}
		})
	}
}

// TestMap_NoContentLeak is the SL3-SEC-3 guard (story AC-7): content present
// in a hook's tool_input (command string / apply_patch body) or tool_response
// must never appear anywhere in the emitted event; not in metadata, not in
// tool.name, not in a span body.
func TestMap_NoContentLeak(t *testing.T) {
	m := testMapper()
	cmdSecret := "SUPER-SECRET-COMMAND-should-not-egress"
	patchSecret := "PATCH-BODY-SECRET-should-not-egress"

	cases := []struct {
		name string
		hook HookName
		ev   *HookEvent
	}{
		{"bash command", HookPreToolUse, &HookEvent{
			SessionID: "th-1", ToolName: "Bash", ToolUseID: "c1",
			ToolInput: json.RawMessage(`{"command":"` + cmdSecret + `"}`),
		}},
		{"apply_patch body", HookPreToolUse, &HookEvent{
			SessionID: "th-1", ToolName: "apply_patch", ToolUseID: "c2",
			ToolInput: json.RawMessage(`{"command":"` + patchSecret + `"}`),
		}},
		{"posttooluse with tool_input", HookPostToolUse, &HookEvent{
			SessionID: "th-1", ToolName: "Bash", ToolUseID: "c3",
			ToolInput: json.RawMessage(`{"command":"` + cmdSecret + `"}`),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := m.Map(tc.hook, tc.ev)
			if !ok {
				t.Fatal("Map ok=false")
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			for _, secret := range []string{cmdSecret, patchSecret} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("content leaked into emitted event: %s", raw)
				}
			}
			if got.Span != nil && (got.Span.RequestBody != "" || got.Span.ResponseBody != "") {
				t.Fatalf("span bodies must be empty (INV-2): %+v", got.Span)
			}
		})
	}
}

// TestMap_PromptCaptureGatedOnContentCapture (story AC-7): the prompt is
// content; carried onto PromptSubmitted only when content-capture is on (ON by
// default at runtime; the mapper default here is off so the gate itself is
// what is under test; CC-adapter parity).
func TestMap_PromptCaptureGatedOnContentCapture(t *testing.T) {
	const prompt = "refactor the auth module"
	e := &HookEvent{SessionID: "th-1", PermissionMode: "default", Prompt: prompt}

	off := testMapper()
	got, ok := off.Map(HookUserPromptSubmit, e)
	if !ok {
		t.Fatal("Map ok=false")
	}
	if got.Content != nil {
		t.Fatalf("content-capture off: prompt must not be captured, got %+v", got.Content)
	}
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), prompt) {
		t.Fatalf("content-capture off: prompt leaked into emitted event: %s", raw)
	}

	on := testMapper()
	on.CaptureContent = true
	got, ok = on.Map(HookUserPromptSubmit, e)
	if !ok {
		t.Fatal("Map ok=false")
	}
	if got.Content == nil || got.Content.Prompt != prompt {
		t.Fatalf("content-capture on: prompt must be captured, got %+v", got.Content)
	}

	got, _ = on.Map(HookUserPromptSubmit, &HookEvent{SessionID: "th-1", PermissionMode: "default"})
	if got.Content != nil {
		t.Fatalf("empty prompt must not set Content, got %+v", got.Content)
	}
}

func TestMap_MetadataStructuralOnly(t *testing.T) {
	m := testMapper()
	got, _ := m.Map(HookSessionStart, &HookEvent{
		SessionID: "th-1", Cwd: "/home/dev/repo", Source: "startup", Model: "gpt-5.3-codex", PermissionMode: "default",
	})
	want := map[string]any{
		"provider": "codex", "source": "startup", "model": "gpt-5.3-codex",
		"cwd": "/home/dev/repo", "permission_mode": "default",
	}
	for k, v := range want {
		if got.Metadata[k] != v {
			t.Errorf("metadata[%q] = %v, want %v", k, got.Metadata[k], v)
		}
	}
	got2, _ := m.Map(HookSessionStart, &HookEvent{SessionID: "th-1"})
	if _, present := got2.Metadata["source"]; present {
		t.Errorf("empty source should be dropped, got %v", got2.Metadata)
	}
	if got2.Metadata["provider"] != "codex" {
		t.Errorf("provider should always be present")
	}

	tool, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "call-1", TurnID: "turn-9", PermissionMode: "default"})
	if tool.Metadata["tool_use_id"] != "call-1" || tool.Metadata["turn_id"] != "turn-9" {
		t.Errorf("tool metadata missing correlation ids: %v", tool.Metadata)
	}
}

func TestMap_DropsUnusablePayloads(t *testing.T) {
	m := testMapper()
	if _, ok := m.Map(HookSessionStart, &HookEvent{SessionID: ""}); ok {
		t.Error("missing session id should drop (ok=false)")
	}
	if _, ok := m.Map(HookSessionStart, nil); ok {
		t.Error("nil event should drop")
	}
	bad := NewMapper(Identity{DeveloperDID: "not-a-did"})
	if _, ok := bad.Map(HookSessionStart, &HookEvent{SessionID: "th-1"}); ok {
		t.Error("non-did:aip identity should drop")
	}
}

func TestMap_UnknownEnumsDropped(t *testing.T) {
	m := testMapper()
	ss, _ := m.Map(HookSessionStart, &HookEvent{SessionID: "th-1", Source: "evil-source", PermissionMode: "pwn"})
	if _, ok := ss.Metadata["source"]; ok {
		t.Errorf("unknown source should be dropped, got %v", ss.Metadata["source"])
	}
	if _, ok := ss.Metadata["permission_mode"]; ok {
		t.Errorf("unknown permission_mode should be dropped")
	}
	se, _ := m.Map(HookSessionEnd, &HookEvent{SessionID: "th-1", Reason: "bogus"})
	if _, ok := se.Metadata["reason"]; ok {
		t.Errorf("unknown reason should be dropped")
	}
	se2, _ := m.Map(HookSessionEnd, &HookEvent{SessionID: "th-1", Reason: "other"})
	if se2.Metadata["reason"] != "other" {
		t.Errorf("reason 'other' should be carried, got %v", se2.Metadata)
	}
}

func TestMap_IdentifiersBounded(t *testing.T) {
	m := testMapper()
	huge := strings.Repeat("A", maxIdentLen+500)
	got, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: huge, ToolUseID: huge})
	if len([]rune(got.Tool.Name)) != maxIdentLen {
		t.Errorf("tool name not bounded: len=%d, want %d", len([]rune(got.Tool.Name)), maxIdentLen)
	}
	if got.Span == nil || len([]rune(got.Span.InvocationID)) != maxIdentLen {
		t.Errorf("invocation id not bounded")
	}
	if len([]rune(got.Metadata["tool_use_id"].(string))) != maxIdentLen {
		t.Errorf("metadata tool_use_id not bounded")
	}
}

// TestDeriveID_ToolUseIDDistinguishes pins the INV-5 improvement Codex
// enables: two otherwise-identical same-instant Bash calls get distinct event
// ids (the tool_use_id rides the invocation slot that feeds deriveID), while
// the same logical event always re-derives the same id.
func TestDeriveID_ToolUseIDDistinguishes(t *testing.T) {
	m := testMapper()
	m.NewID = nil // production derivation
	a1, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "call-1"})
	a2, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "call-1"})
	b, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "call-2"})
	if a1.EventID != a2.EventID {
		t.Errorf("same logical event must derive the same id: %s vs %s", a1.EventID, a2.EventID)
	}
	if a1.EventID == b.EventID {
		t.Errorf("distinct tool_use_ids must derive distinct ids (same tool, same instant)")
	}
	if !strings.HasPrefix(a1.EventID, "cdx-") {
		t.Errorf("codex ids are namespaced cdx-, got %s", a1.EventID)
	}
}

func TestClassifyTool_MalformedMCPFallsBack(t *testing.T) {
	kind, sem, _, server, _ := classifyTool("mcp__")
	if kind != client.ToolShell {
		t.Errorf("malformed mcp name should fall back to shell, got kind=%q", kind)
	}
	if sem != "internal" || server != "" {
		t.Errorf("malformed mcp: sem=%q server=%q, want internal/empty", sem, server)
	}
}

func TestSplitMCPName(t *testing.T) {
	tests := []struct{ in, server, fn string }{
		{"mcp__github__create_issue", "github", "create_issue"},
		{"mcp__memory__create_entities", "memory", "create_entities"},
		{"mcp__srv__ns__deep_tool", "srv", "ns__deep_tool"},
		{"mcp__lonely", "lonely", ""},
	}
	for _, tt := range tests {
		s, f := splitMCPName(tt.in)
		if s != tt.server || f != tt.fn {
			t.Errorf("splitMCPName(%q) = (%q,%q), want (%q,%q)", tt.in, s, f, tt.server, tt.fn)
		}
	}
}
