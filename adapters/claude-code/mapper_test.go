package claudecode

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

const testDID = "did:aip:7f3c9b2e-0000-5000-a000-000000000001"

func testMapper() Mapper {
	m := NewMapper(Identity{DeveloperDID: testDID})
	m.Now = func() time.Time { return time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC) }
	m.NewID = func() string { return "evt-fixed" }
	return m
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
			ev:       &HookEvent{SessionID: "s1", Cwd: "/repo", Source: "startup", Model: "claude-opus-4-8"},
			wantType: client.EventSessionStarted,
			wantTool: client.Tool{Name: "claude-code", Kind: client.ToolShell},
		},
		{
			name:     "prompt submitted",
			hook:     HookUserPromptSubmit,
			ev:       &HookEvent{SessionID: "s1", PermissionMode: "default"},
			wantType: client.EventPromptSubmitted,
			wantTool: client.Tool{Name: "claude-code", Kind: client.ToolShell},
		},
		{
			name:     "pretooluse edit → file_write started",
			hook:     HookPreToolUse,
			ev:       &HookEvent{SessionID: "s1", ToolName: "Edit", ToolInput: json.RawMessage(`{"file_path":"cli/main.go"}`)},
			wantType: client.EventToolCall,
			wantTool: client.Tool{Name: "Edit", Kind: client.ToolFile},
			wantSpan: &client.Span{SemanticType: "file_write", Stage: "started", FilePath: "cli/main.go", FileOp: "edit"},
		},
		{
			name:     "posttooluse read → file_read completed",
			hook:     HookPostToolUse,
			ev:       &HookEvent{SessionID: "s1", ToolName: "Read", ToolInput: json.RawMessage(`{"file_path":"go.mod"}`)},
			wantType: client.EventToolResult,
			wantTool: client.Tool{Name: "Read", Kind: client.ToolFile},
			wantSpan: &client.Span{SemanticType: "file_read", Stage: "completed", FilePath: "go.mod", FileOp: "read"},
		},
		{
			name:     "pretooluse bash → shell internal",
			hook:     HookPreToolUse,
			ev:       &HookEvent{SessionID: "s1", ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"rm -rf /"}`)},
			wantType: client.EventToolCall,
			wantTool: client.Tool{Name: "Bash", Kind: client.ToolShell},
			// A shell call's operation identity is the command, hashed — so a
			// retry of the same command keys to the same activity (and thus the
			// same approval), while a different command never does.
			wantSpan: &client.Span{
				SemanticType: "internal", Stage: "started",
				OperationID: client.OperationForCommand("rm -rf /"),
			},
		},
		{
			name:     "pretooluse mcp → mcp_tool_call",
			hook:     HookPreToolUse,
			ev:       &HookEvent{SessionID: "s1", ToolName: "mcp__github__create_issue"},
			wantType: client.EventToolCall,
			wantTool: client.Tool{Name: "mcp__github__create_issue", Kind: client.ToolMCP, MCPServer: "github"},
			wantSpan: &client.Span{SemanticType: "mcp_tool_call", Stage: "started", MCPServer: "github", Function: "create_issue"},
		},
		{
			name:     "unknown tool → shell internal catch-all",
			hook:     HookPreToolUse,
			ev:       &HookEvent{SessionID: "s1", ToolName: "WebFetch"},
			wantType: client.EventToolCall,
			wantTool: client.Tool{Name: "WebFetch", Kind: client.ToolShell},
			wantSpan: &client.Span{SemanticType: "internal", Stage: "started"},
		},
		{
			name:     "session end",
			hook:     HookSessionEnd,
			ev:       &HookEvent{SessionID: "s1", Reason: "logout"},
			wantType: client.EventSessionEnded,
			wantTool: client.Tool{Name: "claude-code", Kind: client.ToolShell},
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
				t.Errorf("session id = %q, want %q", got.SessionID, tt.ev.SessionID)
			}
			if got.DeveloperDID != testDID {
				t.Errorf("developer_did = %q, want %q", got.DeveloperDID, testDID)
			}
			if got.EventID == "" {
				t.Error("event_id is empty (INV-5)")
			}
			// INV-2: content is NEVER populated by the adapter.
			if got.Content != nil {
				t.Errorf("content must be nil (metadata-only, INV-2), got %+v", got.Content)
			}
			switch {
			case tt.wantSpan == nil && got.Span != nil:
				t.Errorf("expected no span, got %+v", got.Span)
			case tt.wantSpan != nil && got.Span == nil:
				t.Errorf("expected span %+v, got nil", tt.wantSpan)
			case tt.wantSpan != nil:
				if *got.Span != *tt.wantSpan {
					t.Errorf("span = %+v, want %+v", *got.Span, *tt.wantSpan)
				}
			}
		})
	}
}

// TestMap_NoContentLeak is the SL3-SEC-3 guard: content present in a hook's
// tool_input (command, file contents, output) must NEVER appear anywhere in the
// emitted event — not in metadata, not in tool.name, not in a span body. Only
// the structural file_path is carried.
func TestMap_NoContentLeak(t *testing.T) {
	m := testMapper()
	secret := "SUPER-SECRET-PAYLOAD-should-not-egress"
	ev := &HookEvent{
		SessionID: "s1",
		ToolName:  "Write",
		ToolInput: json.RawMessage(`{"file_path":"/tmp/x.txt","content":"` + secret + `"}`),
	}
	got, ok := m.Map(HookPreToolUse, ev)
	if !ok {
		t.Fatal("Map ok=false")
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("content leaked into emitted event: %s", raw)
	}
	// The structural path IS carried (it is not content).
	if got.Span == nil || got.Span.FilePath != "/tmp/x.txt" {
		t.Fatalf("expected file_path carried, got span=%+v", got.Span)
	}
	// Span bodies stay empty (the client would strip them anyway, but the adapter
	// must not set them in the first place).
	if got.Span.RequestBody != "" || got.Span.ResponseBody != "" {
		t.Fatalf("span bodies must be empty (INV-2): %+v", got.Span)
	}
}

// STORY-E7-S7 (OD4): the prompt is CONTENT — carried onto the PromptSubmitted
// event ONLY when content-capture is opted in, never by default.
func TestMap_PromptCaptureGatedOnContentCapture(t *testing.T) {
	const prompt = "refactor the auth module"
	e := &HookEvent{SessionID: "s1", PermissionMode: "default", Prompt: prompt}

	// Default (capture off): the prompt must NOT reach the event.
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

	// Capture on: the prompt is carried on ev.Content.Prompt (→ signal_args downstream).
	on := testMapper()
	on.CaptureContent = true
	got, ok = on.Map(HookUserPromptSubmit, e)
	if !ok {
		t.Fatal("Map ok=false")
	}
	if got.Content == nil || got.Content.Prompt != prompt {
		t.Fatalf("content-capture on: prompt must be captured, got %+v", got.Content)
	}

	// Capture on but empty prompt ⇒ no Content (nothing to carry).
	got, _ = on.Map(HookUserPromptSubmit, &HookEvent{SessionID: "s1", PermissionMode: "default"})
	if got.Content != nil {
		t.Fatalf("empty prompt must not set Content, got %+v", got.Content)
	}
}

func TestMap_MetadataStructuralOnly(t *testing.T) {
	m := testMapper()
	got, _ := m.Map(HookSessionStart, &HookEvent{
		SessionID: "s1", Cwd: "/home/dev/repo", Source: "startup", Model: "claude-opus-4-8", PermissionMode: "default",
	})
	want := map[string]any{
		"provider": "claude-code", "source": "startup", "model": "claude-opus-4-8",
		"cwd": "/home/dev/repo", "permission_mode": "default",
	}
	for k, v := range want {
		if got.Metadata[k] != v {
			t.Errorf("metadata[%q] = %v, want %v", k, got.Metadata[k], v)
		}
	}
	// compact drops empty values.
	got2, _ := m.Map(HookSessionStart, &HookEvent{SessionID: "s1"})
	if _, present := got2.Metadata["source"]; present {
		t.Errorf("empty source should be dropped, got %v", got2.Metadata)
	}
	if got2.Metadata["provider"] != "claude-code" {
		t.Errorf("provider should always be present")
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
	// Bad DID → drop.
	bad := NewMapper(Identity{DeveloperDID: "not-a-did"})
	if _, ok := bad.Map(HookSessionStart, &HookEvent{SessionID: "s1"}); ok {
		t.Error("non-did:aip identity should drop")
	}
}

func TestMap_WorkflowIDIsDIDNotCwd(t *testing.T) {
	m := testMapper()
	// Two hooks in the same session, one WITH cwd and one WITHOUT, must produce
	// the SAME workflow_id (the client derives it from the DID when WorkspaceID
	// is empty) — so a session never fragments on per-hook cwd presence (F4).
	withCwd, _ := m.Map(HookSessionStart, &HookEvent{SessionID: "s1", Cwd: "/repo"})
	noCwd, _ := m.Map(HookUserPromptSubmit, &HookEvent{SessionID: "s1"})
	if withCwd.WorkspaceID != "" || noCwd.WorkspaceID != "" {
		t.Errorf("WorkspaceID should be empty (client uses DID as workflow_id), got %q/%q",
			withCwd.WorkspaceID, noCwd.WorkspaceID)
	}
}

func TestMap_UnknownEnumsDropped(t *testing.T) {
	m := testMapper()
	// A crafted payload with bogus lifecycle enum values must not egress them.
	ss, _ := m.Map(HookSessionStart, &HookEvent{SessionID: "s1", Source: "evil-source", PermissionMode: "pwn"})
	if _, ok := ss.Metadata["source"]; ok {
		t.Errorf("unknown source should be dropped, got %v", ss.Metadata["source"])
	}
	if _, ok := ss.Metadata["permission_mode"]; ok {
		t.Errorf("unknown permission_mode should be dropped")
	}
	se, _ := m.Map(HookSessionEnd, &HookEvent{SessionID: "s1", Reason: "bogus"})
	if _, ok := se.Metadata["reason"]; ok {
		t.Errorf("unknown reason should be dropped")
	}
}

func TestMap_IdentifiersBounded(t *testing.T) {
	m := testMapper()
	huge := strings.Repeat("A", maxIdentLen+500)
	got, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "s1", ToolName: huge})
	if len([]rune(got.Tool.Name)) != maxIdentLen {
		t.Errorf("tool name not bounded: len=%d, want %d", len([]rune(got.Tool.Name)), maxIdentLen)
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

// The outcome derivation (ADR-0018 Decision 1). It is structural: which hook
// fired IS the answer, so these tests assert the mapping and — more importantly
// — that nothing was read out of the tool's own output to get there.
func TestMap_ToolStatusIsDerivedFromWhichHookFired(t *testing.T) {
	m := testMapper()

	ev, ok := m.Map(HookPostToolUse, &HookEvent{
		SessionID: "s", ToolName: "Bash", ToolUseID: "toolu_1",
		ToolInput: json.RawMessage(`{"command":"go vet ./..."}`),
	})
	if !ok {
		t.Fatal("PostToolUse must map")
	}
	if ev.Status != client.StatusCompleted {
		t.Errorf("PostToolUse status = %q, want %q — Claude Code fires this hook only "+
			"after a SUCCESSFUL tool (2.1.229 hook table; failures fire PostToolUseFailure)",
			ev.Status, client.StatusCompleted)
	}

	// The started half has no outcome yet, and must not claim one.
	call, ok := m.Map(HookPreToolUse, &HookEvent{SessionID: "s", ToolName: "Bash", ToolUseID: "toolu_1"})
	if !ok {
		t.Fatal("PreToolUse must map")
	}
	if call.Status != "" {
		t.Errorf("PreToolUse carries status %q; a call that has not run has no outcome", call.Status)
	}
}

// Lifecycle events must never carry an outcome: payload.status writes the row's
// workflow_status column for any event type, where it means something else.
func TestMap_LifecycleEventsCarryNoStatus(t *testing.T) {
	m := testMapper()
	for _, tc := range []struct {
		hook HookName
		ev   *HookEvent
	}{
		{HookSessionStart, &HookEvent{SessionID: "s", Source: "startup"}},
		{HookUserPromptSubmit, &HookEvent{SessionID: "s", Prompt: "hi"}},
		{HookSessionEnd, &HookEvent{SessionID: "s", Reason: "other"}},
	} {
		ev, ok := m.Map(tc.hook, tc.ev)
		if !ok {
			t.Fatalf("%s must map", tc.hook)
		}
		if ev.Status != "" {
			t.Errorf("%s carries status %q; the field is tool-results-only", tc.hook, ev.Status)
		}
	}
}

// INV-2 the structural way: the derivation must not have introduced a path from
// tool output text to the event. A sentinel in tool_response — the field this
// adapter deliberately does not bind — must be nowhere in the emitted event.
func TestMap_StatusDerivationReadsNoToolOutput(t *testing.T) {
	m := testMapper()
	const sentinel = "SENTINEL-TOOL-OUTPUT-must-not-be-read"
	ev, ok := m.Map(HookPostToolUse, &HookEvent{
		SessionID: "s", ToolName: "Bash", ToolUseID: "toolu_2",
		ToolInput: json.RawMessage(`{"command":"echo hi"}`),
	})
	if !ok {
		t.Fatal("must map")
	}
	blob, _ := json.Marshal(ev)
	if strings.Contains(string(blob), sentinel) {
		t.Errorf("tool output reached the event: %s", blob)
	}
	// And the status is present, so the assertion above is not vacuous.
	if ev.Status != client.StatusCompleted {
		t.Errorf("status = %q, want %q", ev.Status, client.StatusCompleted)
	}
}
