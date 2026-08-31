package claudecode

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
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
			if got.Content != nil {
				t.Errorf("content must be nil (metadata-only, INV-2), got %+v", got.Content)
			}
			switch {
			case tt.wantSpan == nil && got.Span != nil:
				t.Errorf("expected no span, got %+v", got.Span)
			case tt.wantSpan != nil && got.Span == nil:
				t.Errorf("expected span %+v, got nil", tt.wantSpan)
			case tt.wantSpan != nil:
				// No EquateEmpty: Span carries RequestHeaders/ResponseHeaders as
				// maps, and an absent header map is not an empty one. The mapper
				// never sets either here, so nil is the shape being asserted.
				if diff := cmp.Diff(*tt.wantSpan, *got.Span); diff != "" {
					t.Errorf("span mismatch (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestMap_NoContentLeak is the content-gate guard: with capture OFF
// (testMapper's default), content present in a hook's tool_input must not
// appear anywhere in the emitted event; not in metadata, not in tool.name, not
// in a span body.
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
	if got.Span == nil || got.Span.FilePath != "/tmp/x.txt" {
		t.Fatalf("expected file_path carried, got span=%+v", got.Span)
	}
	if got.Span.RequestBody != "" || got.Span.ResponseBody != "" {
		t.Fatalf("span bodies must be empty (INV-2): %+v", got.Span)
	}
}

// TestMap_PromptCaptureGatedOnContentCapture story-E7-S7 (OD4): the prompt is
// content; carried onto the PromptSubmitted event only when content-capture is
// opted in, never by default.
func TestMap_PromptCaptureGatedOnContentCapture(t *testing.T) {
	const prompt = "refactor the auth module"
	e := &HookEvent{SessionID: "s1", PermissionMode: "default", Prompt: prompt}

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
	bad := NewMapper(Identity{DeveloperDID: "not-a-did"})
	if _, ok := bad.Map(HookSessionStart, &HookEvent{SessionID: "s1"}); ok {
		t.Error("non-did:aip identity should drop")
	}
}

func TestMap_WorkflowIDIsDIDNotCwd(t *testing.T) {
	m := testMapper()
	withCwd, _ := m.Map(HookSessionStart, &HookEvent{SessionID: "s1", Cwd: "/repo"})
	noCwd, _ := m.Map(HookUserPromptSubmit, &HookEvent{SessionID: "s1"})
	if withCwd.WorkspaceID != "" || noCwd.WorkspaceID != "" {
		t.Errorf("WorkspaceID should be empty (client uses DID as workflow_id), got %q/%q",
			withCwd.WorkspaceID, noCwd.WorkspaceID)
	}
}

func TestMap_UnknownEnumsDropped(t *testing.T) {
	m := testMapper()
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

// TestMapTurn_AssistantTextIsGatedOnContentCapture the assistant-turn content
// attach.
func TestMapTurn_AssistantTextIsGatedOnContentCapture(t *testing.T) {
	const answer = "I refactored the spool."
	window := turnWindow{HasUsage: true, Model: "claude-opus-4-8"}

	for _, tc := range []struct {
		name    string
		capture bool
		message string
		want    string // "" = expect no Content at all
	}{
		{"capture off", false, answer, ""},
		{"capture on, hook carried nothing", true, "", ""},
		{"capture on, message present", true, answer, answer},
	} {
		m := testMapper()
		m.CaptureContent = tc.capture
		_, completed, ok := m.MapTurn(&HookEvent{SessionID: "s", LastAssistantMessage: tc.message}, window, 0)
		if !ok {
			t.Fatalf("%s: MapTurn not ok", tc.name)
		}
		switch {
		case tc.want == "":
			if completed.Content != nil {
				t.Errorf("%s: content attached anyway: %+v", tc.name, completed.Content)
			}
			if blob, _ := json.Marshal(completed); strings.Contains(string(blob), answer) {
				t.Errorf("%s: assistant text present in the event: %s", tc.name, blob)
			}
		default:
			if completed.Content == nil || completed.Content.Output != tc.want {
				t.Errorf("%s: content = %+v, want Output %q", tc.name, completed.Content, tc.want)
			}
		}
	}
}

// TestMapTurn_StartedHalfNeverCarriesText the text rides the completed half
// only.
func TestMapTurn_StartedHalfNeverCarriesText(t *testing.T) {
	m := testMapper()
	m.CaptureContent = true
	started, _, ok := m.MapTurn(&HookEvent{SessionID: "s", LastAssistantMessage: "the answer"},
		turnWindow{HasUsage: true}, 0)
	if !ok {
		t.Fatal("MapTurn not ok")
	}
	if started.Content != nil {
		t.Errorf("the started half carries content: %+v", started.Content)
	}
}

// TestMapTurn_RedactionIsStructural redaction is a collaborator, so every
// attach path goes through it by construction rather than by remembering to
// call it.
func TestMapTurn_RedactionIsStructural(t *testing.T) {
	const secret = "${OPENBOX_REDACTED_AWS_KEY}"
	window := turnWindow{HasUsage: true}

	redacting := testMapper()
	redacting.CaptureContent = true
	redacting.RedactContent = func(s string) string { return strings.ReplaceAll(s, secret, "[REDACTED]") }
	_, completed, ok := redacting.MapTurn(&HookEvent{
		SessionID: "s", LastAssistantMessage: "your key is " + secret,
	}, window, 0)
	if !ok {
		t.Fatal("MapTurn not ok")
	}
	if strings.Contains(completed.Content.Output, secret) {
		t.Errorf("the redactor did not run before attachment: %q", completed.Content.Output)
	}
	if !strings.Contains(completed.Content.Output, "[REDACTED]") {
		t.Errorf("expected the redaction placeholder, got %q", completed.Content.Output)
	}

	off := testMapper()
	off.CaptureContent = true
	_, plain, _ := off.MapTurn(&HookEvent{SessionID: "s", LastAssistantMessage: "your key is " + secret}, window, 0)
	if !strings.Contains(plain.Content.Output, secret) {
		t.Error("with no redactor wired the text must pass through unchanged; a silent " +
			"partial redaction would be worse than the documented opt-out")
	}
}

// TestMap_ToolStatusIsDerivedFromWhichHookFired the outcome derivation.
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
		t.Errorf("PostToolUse status = %q, want %q; Claude Code fires this hook only "+
			"after a SUCCESSFUL tool (2.1.229 hook table; failures fire PostToolUseFailure)",
			ev.Status, client.StatusCompleted)
	}

	call, ok := m.Map(HookPreToolUse, &HookEvent{SessionID: "s", ToolName: "Bash", ToolUseID: "toolu_1"})
	if !ok {
		t.Fatal("PreToolUse must map")
	}
	if call.Status != "" {
		t.Errorf("PreToolUse carries status %q; a call that has not run has no outcome", call.Status)
	}
}

// TestMap_LifecycleEventsCarryNoStatus lifecycle events must never carry an
// outcome: payload.status writes the row's workflow_status column for any
// event type, where it means something else.
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

// `status` is structural: it is derived from which hook fired, never parsed
// out of what the tool produced.
func TestMap_StatusDerivationReadsNoToolOutput(t *testing.T) {
	m := testMapper() // content capture OFF
	const sentinel = "SENTINEL-TOOL-OUTPUT-must-not-be-read"
	ev, ok := m.Map(HookPostToolUse, &HookEvent{
		SessionID: "s", ToolName: "Bash", ToolUseID: "toolu_2",
		ToolInput:    json.RawMessage(`{"command":"echo hi"}`),
		ToolResponse: json.RawMessage(`{"stdout":"` + sentinel + `"}`),
	})
	if !ok {
		t.Fatal("must map")
	}
	blob, _ := json.Marshal(ev)
	if strings.Contains(string(blob), sentinel) {
		t.Errorf("tool output reached the event with capture off: %s", blob)
	}
	if ev.Status != client.StatusCompleted {
		t.Errorf("status = %q, want %q", ev.Status, client.StatusCompleted)
	}

	on := testMapper()
	on.CaptureContent = true
	got, ok := on.Map(HookPostToolUse, &HookEvent{
		SessionID: "s", ToolName: "Bash", ToolUseID: "toolu_2b",
		ToolInput:    json.RawMessage(`{"command":"echo hi"}`),
		ToolResponse: json.RawMessage(`{"stdout":"error: everything failed"}`),
	})
	if !ok {
		t.Fatal("must map")
	}
	if got.Status != client.StatusCompleted {
		t.Errorf("status = %q with capture on, want %q; the outcome comes from WHICH "+
			"hook fired, never from what the tool printed", got.Status, client.StatusCompleted)
	}
}

// TestMap_ToolOutputIsGatedOnContentCapture the gate on tool output, at the
// mapper boundary (the wire is C32/C33).
func TestMap_ToolOutputIsGatedOnContentCapture(t *testing.T) {
	const body = "total 4\ndrwxr-xr-x 2 root root"
	e := &HookEvent{
		SessionID: "s", ToolName: "Bash", ToolUseID: "toolu_g1",
		ToolInput:    json.RawMessage(`{"command":"ls -la"}`),
		ToolResponse: json.RawMessage(`{"stdout":"` + body + `"}`),
	}

	off := testMapper()
	got, ok := off.Map(HookPostToolUse, e)
	if !ok {
		t.Fatal("must map")
	}
	if got.Content != nil {
		t.Errorf("capture off: tool output must not be captured, got %+v", got.Content)
	}

	on := testMapper()
	on.CaptureContent = true
	got, ok = on.Map(HookPostToolUse, e)
	if !ok {
		t.Fatal("must map")
	}
	if got.Content == nil || !strings.Contains(got.Content.ToolOutput, "drwxr-xr-x") {
		t.Fatalf("capture on: tool output not carried on Content.ToolOutput, got %+v", got.Content)
	}
	if got.Content.Output != "" {
		t.Errorf("tool output landed on Content.Output, which carries TURN text: %q",
			got.Content.Output)
	}
}

// TestMap_PostToolUseFailureIsACompletedActivityThatFailed a failed call is
// the same wire event as a successful one; a completed activity with a
// different outcome; so it pairs with its ActivityStarted and is visible to
// every consumer that reads ActivityCompleted.
func TestMap_PostToolUseFailureIsACompletedActivityThatFailed(t *testing.T) {
	m := testMapper()
	ev, ok := m.Map(HookPostToolUseFailure, &HookEvent{
		SessionID: "s", ToolName: "Bash", ToolUseID: "toolu_f1",
		ToolInput: json.RawMessage(`{"command":"exit 3"}`),
		ErrorType: "Error: exit code 3",
	})
	if !ok {
		t.Fatal("PostToolUseFailure must map")
	}
	if ev.EventType != client.EventToolResult {
		t.Errorf("event type = %q, want %q; a failed call still completed",
			ev.EventType, client.EventToolResult)
	}
	if ev.Status != client.StatusFailed {
		t.Errorf("status = %q, want %q", ev.Status, client.StatusFailed)
	}
	if ev.EndedAt == "" {
		t.Error("ended_at unset; without it the completed half reports no duration")
	}
	if ev.Span == nil || ev.Span.InvocationID != "toolu_f1" {
		t.Errorf("invocation id not carried: %+v", ev.Span)
	}
}

// TestMap_IsInterruptIsTriState is_interrupt is tri-state on purpose: a
// cancelled call and a broken tool are both `failed`, and "the provider did
// not say" is a third answer.
func TestMap_IsInterruptIsTriState(t *testing.T) {
	m := testMapper()
	yes, no := true, false
	for _, tc := range []struct {
		name        string
		in          *bool
		want        any
		wantPresent bool
	}{
		{"absent", nil, nil, false},
		{"false", &no, false, true},
		{"true", &yes, true, true},
	} {
		ev, ok := m.Map(HookPostToolUseFailure, &HookEvent{
			SessionID: "s", ToolName: "Bash", ToolUseID: "toolu_i", IsInterrupt: tc.in,
		})
		if !ok {
			t.Fatalf("%s: must map", tc.name)
		}
		got, present := ev.Metadata["is_interrupt"]
		if present != tc.wantPresent {
			t.Errorf("%s: is_interrupt present = %t, want %t", tc.name, present, tc.wantPresent)
		}
		if present && got != tc.want {
			t.Errorf("%s: is_interrupt = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMap_LifecycleSignals(t *testing.T) {
	m := testMapper()

	sub, ok := m.Map(HookSubagentStart, &HookEvent{
		SessionID: "s", AgentID: "agt-1", AgentType: "code-reviewer",
	})
	if !ok {
		t.Fatal("SubagentStart must map")
	}
	if sub.EventType != client.EventSubagentStarted {
		t.Errorf("event type = %q, want %q", sub.EventType, client.EventSubagentStarted)
	}
	if sub.Metadata["agent_type"] != "code-reviewer" {
		t.Errorf("agent_type not carried: %v", sub.Metadata)
	}

	den, ok := m.Map(HookPermissionDenied, &HookEvent{
		SessionID: "s", ToolName: "Bash", ToolUseID: "toolu_d1",
		ToolInput: json.RawMessage(`{"command":"rm -rf /"}`),
	})
	if !ok {
		t.Fatal("PermissionDenied must map")
	}
	if den.EventType != client.EventPermissionDenied {
		t.Errorf("event type = %q, want %q", den.EventType, client.EventPermissionDenied)
	}
	if den.Metadata["tool_use_id"] != "toolu_d1" {
		t.Errorf("tool_use_id not carried; the denial cannot be correlated with its call: %v", den.Metadata)
	}

	for _, in := range []string{"rate_limit", "billing_error", "max_output_tokens", "unknown"} {
		ev, ok := m.Map(HookStopFailure, &HookEvent{SessionID: "s", ErrorType: in})
		if !ok {
			t.Fatalf("StopFailure(%s) must map", in)
		}
		if ev.EventType != client.EventAPIError {
			t.Errorf("event type = %q, want %q", ev.EventType, client.EventAPIError)
		}
		if ev.Metadata["error_type"] != in {
			t.Errorf("error_type = %v, want %q", ev.Metadata["error_type"], in)
		}
	}
}

// `error` is the same JSON key on TWO hooks: a closed enum on StopFailure, and
// free text a tool wrote on PostToolUseFailure.
func TestMap_FreeTextErrorNeverEgresses(t *testing.T) {
	m := testMapper() // content capture OFF
	const leaky = "ENOENT: no such file /home/dev/.ssh/id_rsa"

	failed, ok := m.Map(HookPostToolUseFailure, &HookEvent{
		SessionID: "s", ToolName: "Read", ToolUseID: "toolu_e1", ErrorType: leaky,
	})
	if !ok {
		t.Fatal("must map")
	}
	if blob, _ := json.Marshal(failed); strings.Contains(string(blob), leaky) {
		t.Errorf("PostToolUseFailure's free-text error reached the event with capture off: %s", blob)
	}

	on := testMapper()
	on.CaptureContent = true
	captured, ok := on.Map(HookPostToolUseFailure, &HookEvent{
		SessionID: "s", ToolName: "Read", ToolUseID: "toolu_e2", ErrorType: leaky,
	})
	if !ok {
		t.Fatal("must map")
	}
	if captured.Content == nil || captured.Content.ToolOutput != leaky {
		t.Errorf("capture on: the tool's error text must ride Content.ToolOutput, got %+v",
			captured.Content)
	}
	if v, present := captured.Metadata["error_type"]; present {
		t.Errorf("error_type = %v; free text must never reach the provider-enum field, "+
			"gated or not", v)
	}

	apiErr, ok := m.Map(HookStopFailure, &HookEvent{SessionID: "s", ErrorType: leaky})
	if !ok {
		t.Fatal("must map")
	}
	if v, present := apiErr.Metadata["error_type"]; present {
		t.Errorf("error_type = %v; a value outside the provider enum must be dropped", v)
	}
	if blob, _ := json.Marshal(apiErr); strings.Contains(string(blob), leaky) {
		t.Errorf("free text reached the APIError event: %s", blob)
	}
}

// TestMap_TaskSubagentTypeIsCarriedButNotItsPrompt subagent_type names which
// agent kind was spawned; an identifier chosen from the installed set, not
// text the model wrote. Its neighbours in the same tool_input, `prompt` and
// `description`, are free text and must stay unread.
func TestMap_TaskSubagentTypeIsCarriedButNotItsPrompt(t *testing.T) {
	m := testMapper()
	const secretPrompt = "SUBAGENT-PROMPT-must-not-egress"
	ev, ok := m.Map(HookPreToolUse, &HookEvent{
		SessionID: "s", ToolName: "Task", ToolUseID: "toolu_t1",
		ToolInput: json.RawMessage(`{"subagent_type":"code-reviewer","description":"review","prompt":"` + secretPrompt + `"}`),
	})
	if !ok {
		t.Fatal("Task call must map")
	}
	if ev.Metadata["subagent_type"] != "code-reviewer" {
		t.Errorf("subagent_type not carried; every Task call reads as an anonymous "+
			"`tool_name: Task`: %v", ev.Metadata)
	}
	if blob, _ := json.Marshal(ev); strings.Contains(string(blob), secretPrompt) {
		t.Errorf("the subagent prompt egressed: %s", blob)
	}
	other, _ := m.Map(HookPreToolUse, &HookEvent{
		SessionID: "s", ToolName: "Read", ToolUseID: "toolu_t2",
		ToolInput: json.RawMessage(`{"file_path":"/tmp/a"}`),
	})
	if _, present := other.Metadata["subagent_type"]; present {
		t.Errorf("non-Task call carries subagent_type: %v", other.Metadata)
	}
}
