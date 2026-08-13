package claudecode

import (
	"testing"
)

// E8-S3: Claude Code carries tool_use_id on both PreToolUse and PostToolUse, so
// a call's two events pair by identity rather than by a (session, tool, locator)
// heuristic that collided for two identical sequential calls.
//
// It rides Span.InvocationID. It used to ride Span.Function, which also fed
// activity_id — and that was the approval-loop defect: activity_id is the
// approval key, so a per-invocation value meant an approved request could never
// be consumed by the retry.
func TestCorrelation_ToolUseIDRidesTheInvocationSlot(t *testing.T) {
	m := testMapper()

	for _, hook := range []HookName{HookPreToolUse, HookPostToolUse} {
		ev, ok := m.Map(hook, &HookEvent{
			SessionID: "s1", ToolName: "Bash", ToolUseID: "toolu_01", PermissionMode: "default",
			ToolInput: []byte(`{"command":"ls -la"}`),
		})
		if !ok {
			t.Fatalf("%s did not map", hook)
		}
		if ev.Span.InvocationID != "toolu_01" {
			t.Errorf("%s: tool_use_id should ride span.invocation_id, got %q", hook, ev.Span.InvocationID)
		}
		// The operation must NOT be the invocation for a gated class.
		if ev.Span.OperationID == "toolu_01" {
			t.Errorf("%s: operation id is the invocation — an approval could not survive a retry", hook)
		}
		if ev.Metadata["tool_use_id"] != "toolu_01" {
			t.Errorf("%s: tool_use_id should be audit-visible in metadata, got %v", hook, ev.Metadata)
		}
	}

	// A file tool keeps its structural path and gains the invocation id — the
	// two occupy different fields, so neither is lost. Its operation falls back
	// to the invocation: file classes are not gated, and discriminating them by
	// path would collapse repeated edits of one file into a single activity.
	ev, _ := m.Map(HookPreToolUse, &HookEvent{
		SessionID: "s1", ToolName: "Read", ToolUseID: "toolu_02",
		ToolInput: []byte(`{"file_path":"/a.go"}`),
	})
	if ev.Span.FilePath != "/a.go" || ev.Span.InvocationID != "toolu_02" {
		t.Errorf("file tool should carry both locator and invocation id, got path=%q invocation=%q",
			ev.Span.FilePath, ev.Span.InvocationID)
	}
	if ev.Span.Function != "" {
		t.Errorf("span.function is the MCP function name only, got %q", ev.Span.Function)
	}

	// Without the field (older Claude Code) the heuristic derivation stands.
	legacy, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "s1", ToolName: "Read"})
	if legacy.Span.InvocationID != "" {
		t.Errorf("absent tool_use_id must leave span.invocation_id empty, got %q", legacy.Span.InvocationID)
	}
}

// The invariant the whole approval loop rests on: any class the gate escalates
// can hold an approval, and an approval is keyed on activity_id — so every such
// class must have an operation identity that survives a retry.
//
// Since ADR-0017 the gate escalates EVERY class, so the name no longer means
// "the only classes that can hold an approval". These two are the classes with a
// real structural discriminator, and they are the ones that must stay
// retry-stable; the rest are pinned by
// TestUngatedClassesKeepInvocationScopedIdentity below, which records a known
// limitation rather than an invariant.
func TestHighRiskClassesHaveAStableOperationID(t *testing.T) {
	m := testMapper()
	gated := []struct {
		tool  string
		input string
	}{
		{"Bash", `{"command":"ls -la"}`},
		{"mcp__github__create_issue", `{"title":"x"}`},
	}
	for _, g := range gated {
		if !isHighRiskClass(g.tool) {
			t.Fatalf("%s lost its structural discriminator — update this table to match operationID", g.tool)
		}
		first, ok := m.Map(HookPreToolUse, &HookEvent{
			SessionID: "s1", ToolName: g.tool, ToolUseID: "toolu_first", ToolInput: []byte(g.input),
		})
		if !ok {
			t.Fatalf("%s did not map", g.tool)
		}
		retry, _ := m.Map(HookPreToolUse, &HookEvent{
			SessionID: "s1", ToolName: g.tool, ToolUseID: "toolu_retry", ToolInput: []byte(g.input),
		})
		if first.Span.OperationID == "" || first.Span.OperationID != retry.Span.OperationID {
			t.Errorf("%s: gated class has no retry-stable operation id (%q vs %q) — "+
				"an approved request could never be consumed",
				g.tool, first.Span.OperationID, retry.Span.OperationID)
		}
		if first.Span.OperationID == first.Span.InvocationID {
			t.Errorf("%s: operation id is the invocation — see the fallback in operationID", g.tool)
		}
	}
}

// A known limitation, asserted so it stays deliberate. ADR-0017 gates every
// class, but only shell and MCP have a structural discriminator; the rest key on
// the invocation, so their approval identity moves on every retry. An approval
// granted for one of these calls is therefore never consumed — the developer
// retries and a fresh request is filed instead.
//
// This is pinned rather than fixed because the fix is to change operationID,
// which changes activity_id — this product's event identity, byte-pinned in
// client/approval_key_pin_test.go and relied on by core's dedupe. That is its
// own decision.
//
// What makes the limitation tolerable is its direction, which this test also
// pins: the id moving means an approval cannot be MATCHED, so the call is
// re-asked. It can never be silently granted.
func TestUngatedClassesKeepInvocationScopedIdentity(t *testing.T) {
	m := testMapper()
	for _, tool := range []string{"Write", "Read", "WebFetch"} {
		first, ok := m.Map(HookPreToolUse, &HookEvent{
			SessionID: "s1", ToolName: tool, ToolUseID: "toolu_first",
			ToolInput: []byte(`{"file_path":"/tmp/x","content":"y"}`),
		})
		if !ok {
			t.Fatalf("%s did not map", tool)
		}
		retry, _ := m.Map(HookPreToolUse, &HookEvent{
			SessionID: "s1", ToolName: tool, ToolUseID: "toolu_retry",
			ToolInput: []byte(`{"file_path":"/tmp/x","content":"y"}`),
		})
		if first.Span.OperationID != first.Span.InvocationID {
			t.Errorf("%s: expected the invocation fallback; it gained a discriminator, so "+
				"the limitation above is stale and the approval loop now works for it", tool)
		}
		if first.Span.OperationID == retry.Span.OperationID {
			t.Errorf("%s: identity survived a retry, which contradicts the documented "+
				"limitation — update the comment on operationID", tool)
		}
	}
}

// The other half: a gated class must not OVER-grant either. Approving one
// command must never carry to a different one, and core says the same about MCP
// arguments ("same tool with different arguments must require fresh approval").
func TestGatedOperationsAreDistinctPerRequest(t *testing.T) {
	m := testMapper()
	for _, c := range []struct{ name, tool, a, b string }{
		{"shell command", "Bash", `{"command":"ls -la"}`, `{"command":"rm -rf /"}`},
		{"mcp arguments", "mcp__github__create_issue", `{"title":"a"}`, `{"title":"b"}`},
	} {
		first, _ := m.Map(HookPreToolUse, &HookEvent{
			SessionID: "s1", ToolName: c.tool, ToolUseID: "toolu_1", ToolInput: []byte(c.a),
		})
		other, _ := m.Map(HookPreToolUse, &HookEvent{
			SessionID: "s1", ToolName: c.tool, ToolUseID: "toolu_2", ToolInput: []byte(c.b),
		})
		if first.Span.OperationID == other.Span.OperationID {
			t.Errorf("%s: two different requests share an operation id — approving one would grant the other", c.name)
		}
	}

	// Key order and whitespace are not a different operation; a retry that
	// re-serializes its arguments must still match.
	a, _ := m.Map(HookPreToolUse, &HookEvent{
		SessionID: "s1", ToolName: "mcp__github__create_issue", ToolUseID: "t1",
		ToolInput: []byte(`{"title":"x","body":"y"}`),
	})
	b, _ := m.Map(HookPreToolUse, &HookEvent{
		SessionID: "s1", ToolName: "mcp__github__create_issue", ToolUseID: "t2",
		ToolInput: []byte(`{ "body" : "y" ,  "title" : "x" }`),
	})
	if a.Span.OperationID != b.Span.OperationID {
		t.Error("semantically identical MCP arguments produced different operation ids — a retry would re-ask")
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
