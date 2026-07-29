package claudecode

import (
	"testing"
)

// E8-S3: Claude Code carries tool_use_id on both PreToolUse and PostToolUse, so
// a call's two events can pair by identity instead of by the (session, tool,
// locator) heuristic — which collided for two identical sequential calls (the
// limitation documented on client.activityPairKey). The adapter's job is to put
// the id in the locator slot; the derivation itself is the client's, covered by
// TestHookPayload_TraceAndActivityScoping and
// TestHookPayload_SpanFunctionIsNotWireDataForNonMCP.
func TestCorrelation_ToolUseIDRidesPairingSlot(t *testing.T) {
	m := testMapper()

	for _, hook := range []HookName{HookPreToolUse, HookPostToolUse} {
		ev, ok := m.Map(hook, &HookEvent{
			SessionID: "s1", ToolName: "Bash", ToolUseID: "toolu_01", PermissionMode: "default",
		})
		if !ok {
			t.Fatalf("%s did not map", hook)
		}
		if ev.Span.Function != "toolu_01" {
			t.Errorf("%s: tool_use_id should ride span.function, got %q", hook, ev.Span.Function)
		}
		if ev.Metadata["tool_use_id"] != "toolu_01" {
			t.Errorf("%s: tool_use_id should be audit-visible in metadata, got %v", hook, ev.Metadata)
		}
	}

	// A file tool keeps its structural path and gains the pairing id — the two
	// occupy different fields, so neither is lost.
	ev, _ := m.Map(HookPreToolUse, &HookEvent{
		SessionID: "s1", ToolName: "Read", ToolUseID: "toolu_02",
		ToolInput: []byte(`{"file_path":"/a.go"}`),
	})
	if ev.Span.FilePath != "/a.go" || ev.Span.Function != "toolu_02" {
		t.Errorf("file tool should carry both locator and pairing id, got path=%q function=%q",
			ev.Span.FilePath, ev.Span.Function)
	}

	// Without the field (older Claude Code) the heuristic derivation stands.
	legacy, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "s1", ToolName: "Bash"})
	if legacy.Span.Function != "" {
		t.Errorf("absent tool_use_id must leave span.function empty, got %q", legacy.Span.Function)
	}
}

// MCP tools keep the real function name on span.function because for them that
// field IS wire data (mcp_tool); tool_use_id rides metadata only.
func TestCorrelation_MCPKeepsFunctionName(t *testing.T) {
	m := testMapper()
	ev, ok := m.Map(HookPreToolUse, &HookEvent{
		SessionID: "s1", ToolName: "mcp__github__create_issue", ToolUseID: "toolu_09",
	})
	if !ok {
		t.Fatal("MCP tool call did not map")
	}
	if ev.Span.Function != "create_issue" {
		t.Errorf("MCP span.function = %q, want the MCP function name", ev.Span.Function)
	}
	if ev.Span.MCPServer != "github" {
		t.Errorf("MCP span.mcp_server = %q, want github", ev.Span.MCPServer)
	}
	if ev.Metadata["tool_use_id"] != "toolu_09" {
		t.Errorf("MCP events should still carry tool_use_id in metadata, got %v", ev.Metadata)
	}
}

// The subagent ids ride every payload fired inside a subagent, which is what
// makes the tree reconstructable without inventing a lifecycle type for the
// Subagent* boundary markers (COVERAGE.md §3.2).
func TestCorrelation_SubagentIDsInMetadata(t *testing.T) {
	m := testMapper()
	for _, hook := range []HookName{HookPreToolUse, HookPostToolUse, HookUserPromptSubmit} {
		ev, ok := m.Map(hook, &HookEvent{
			SessionID: "s1", ToolName: "Read", AgentID: "agt_7", AgentType: "code-reviewer",
		})
		if !ok {
			t.Fatalf("%s did not map", hook)
		}
		if ev.Metadata["agent_id"] != "agt_7" || ev.Metadata["agent_type"] != "code-reviewer" {
			t.Errorf("%s metadata missing subagent ids: %v", hook, ev.Metadata)
		}
	}
	// Main-agent events carry neither (compact drops the empty values).
	ev, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "s1", ToolName: "Read"})
	if _, ok := ev.Metadata["agent_id"]; ok {
		t.Errorf("main-agent event should carry no agent_id, got %v", ev.Metadata)
	}
}

// The new fields are structural identifiers and must be bounded at the
// untrusted boundary like every other externally-influenced id (maxIdentLen).
func TestCorrelation_IdentifiersBounded(t *testing.T) {
	m := testMapper()
	long := ""
	for len(long) < maxIdentLen*2 {
		long += "x"
	}
	ev, _ := m.Map(HookPreToolUse, &HookEvent{
		SessionID: "s1", ToolName: "Bash", ToolUseID: long, AgentID: long, AgentType: long,
	})
	for _, f := range []struct {
		name string
		got  string
	}{
		{"span.function", ev.Span.Function},
		{"metadata.tool_use_id", ev.Metadata["tool_use_id"].(string)},
		{"metadata.agent_id", ev.Metadata["agent_id"].(string)},
		{"metadata.agent_type", ev.Metadata["agent_type"].(string)},
	} {
		if len(f.got) > maxIdentLen {
			t.Errorf("%s not bounded: len %d > %d", f.name, len(f.got), maxIdentLen)
		}
	}
}
