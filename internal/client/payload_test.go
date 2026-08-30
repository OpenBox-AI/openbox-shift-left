package client

import (
	"encoding/json"
	"testing"
)

// decodePayload builds and re-parses a payload as core would receive it.
func decodePayload(t *testing.T, ev DevEvent) governanceEventPayload {
	t.Helper()
	b, err := buildPayload(ev)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	var p governanceEventPayload
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return p
}

// decodeRaw builds a payload and decodes it as the untyped map core receives,
// so a test can assert a key is ABSENT. The typed struct cannot: a field that
// does not exist on it decodes to nothing either way.
func decodeRaw(t *testing.T, ev DevEvent) map[string]any {
	t.Helper()
	b, err := buildPayload(ev)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

// assertNoSpanKeys holds the accepted trade-off of modelling a tool call as an
// activity: the span layer is retired, so nothing the client emits may carry a
// span or the hook envelope that used to wrap one. A key reappearing here means
// the retired machinery grew a caller again.
//
// ADR-0018 carved out exactly ONE exception — a TurnCompleted carrying assistant
// text under content capture — and this helper is deliberately NOT relaxed to
// accommodate it. It is still applied to every tool and lifecycle payload, and
// the turn case has its own tests that assert the span's shape positively. The
// carve-out stays a carve-out: if a tool payload ever grows `spans`, this fails,
// which is the whole point.
//
// hook_trigger stays forbidden EVERYWHERE, turn span included: a payload with
// hook_trigger true and spans present enters core's approval-bypass fingerprint
// path (governance_workflow.go:310-330).
func assertNoSpanKeys(t *testing.T, m map[string]any) {
	t.Helper()
	for _, k := range []string{"spans", "span_count", "hook_trigger"} {
		if v, present := m[k]; present {
			t.Errorf("payload carries retired key %q = %v", k, v)
		}
	}
}

// rawMeta re-parses the payload's metadata json.RawMessage into a map.
func rawMeta(t *testing.T, p governanceEventPayload) map[string]any {
	t.Helper()
	if len(p.Metadata) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(p.Metadata, &m); err != nil {
		t.Fatalf("metadata unmarshal: %v", err)
	}
	return m
}

// TestBuildPayload_ActivityType asserts the pass-through activity_type the
// openbox-fe dashboard's "Activity" column reads. It is derived from persisted
// fields (EventType + Tool.Name) so it survives the adapter's spool round-trip: a
// tool event carries the specific tool name; everything else carries the
// event_type — never empty (empty would render Activity="Unknown").
func TestBuildPayload_ActivityType(t *testing.T) {
	// Tool events → the specific tool name (survives spooling; tool.name is on the
	// wire, unlike any json:"-" field).
	for _, et := range []EventType{EventToolCall, EventToolResult} {
		p := decodePayload(t, DevEvent{
			EventID: "e1", EventType: et, SessionID: "s1", DeveloperDID: "did:aip:abc",
			Tool: Tool{Name: "Edit", Kind: ToolFile},
		})
		if p.ActivityType != "Edit" {
			t.Errorf("%s activity_type = %q, want %q", et, p.ActivityType, "Edit")
		}
	}
	// Lifecycle → the event_type string (NOT the "claude-code" tool name), never empty.
	life := decodePayload(t, DevEvent{
		EventID: "e2", EventType: EventSessionStarted, SessionID: "s1", DeveloperDID: "did:aip:abc",
		Tool: Tool{Name: "claude-code", Kind: ToolShell},
	})
	if life.ActivityType != string(EventSessionStarted) {
		t.Errorf("lifecycle activity_type = %q, want %q", life.ActivityType, EventSessionStarted)
	}
	// Deploy (git action, no tool-name label) → "Deploy".
	dep := decodePayload(t, DevEvent{
		EventID: "e3", EventType: EventDeploy, SessionID: "s1", DeveloperDID: "did:aip:abc",
		Tool: Tool{Name: "openbox-git-action", Kind: ToolShell},
	})
	if dep.ActivityType != string(EventDeploy) {
		t.Errorf("deploy activity_type = %q, want %q", dep.ActivityType, EventDeploy)
	}
	// A tool event with no tool name falls back to the event_type (never empty).
	nameless := decodePayload(t, DevEvent{
		EventID: "e4", EventType: EventToolCall, SessionID: "s1", DeveloperDID: "did:aip:abc",
	})
	if nameless.ActivityType != string(EventToolCall) {
		t.Errorf("nameless tool activity_type = %q, want %q", nameless.ActivityType, EventToolCall)
	}
}

func TestBuildPayload_Envelope(t *testing.T) {
	ev := DevEvent{
		EventID:      "e1",
		EventType:    EventSessionStarted,
		SessionID:    "run-1",
		DeveloperDID: "did:aip:abc",
		WorkspaceID:  "repo-x", // explicit workspace overrides the DID fallback
		Timestamp:    "2026-07-08T00:00:00Z",
		Tool:         Tool{Name: "session", Kind: ToolShell},
		Metadata:     map[string]any{"provider": "claude-code", "repo": "repo-x"},
	}
	p := decodePayload(t, ev)
	if p.Source != "developer-runtime" {
		t.Errorf("source = %q", p.Source)
	}
	if p.WorkflowID != "repo-x" || p.RunID != "run-1" {
		t.Errorf("(workflow_id, run_id) = (%q,%q)", p.WorkflowID, p.RunID)
	}
	if p.Timestamp != "2026-07-08T00:00:00Z" {
		t.Errorf("timestamp = %q (must pass through verbatim)", p.Timestamp)
	}
	assertNoSpanKeys(t, decodeRaw(t, ev))
	m := rawMeta(t, p)
	if m["provider"] != "claude-code" {
		t.Errorf("metadata.provider = %v", m["provider"])
	}
}

func TestBuildPayload_TokensAndCostToMetadata(t *testing.T) {
	in, out, total := 100, 50, 150
	ev := DevEvent{
		EventID: "e1", EventType: EventPromptSubmitted, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-08T00:00:00Z", Tool: Tool{Name: "prompt", Kind: ToolShell},
		Tokens: &Tokens{Input: &in, Output: &out, Total: &total},
		Cost:   &Cost{Amount: 0.0021, Currency: "USD"},
	}
	m := rawMeta(t, decodePayload(t, ev))
	if m["tokens"] == nil || m["cost"] == nil {
		t.Fatalf("tokens/cost must ride in metadata (no first-class fields); got %v", m)
	}
	tk := m["tokens"].(map[string]any)
	if tk["total"].(float64) != 150 {
		t.Errorf("metadata.tokens.total = %v", tk["total"])
	}
}

// TestBuildPayload_ToolCallIsActivityStarted holds the started half of the
// activity lifecycle: the wire type, the pairing/approval id, and the structural
// input core runs Guardrails stage "0" over.
func TestBuildPayload_ToolCallIsActivityStarted(t *testing.T) {
	ev := DevEvent{
		EventID: "e1", EventType: EventToolCall, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-08T00:00:01Z",
		StartedAt: "2026-07-08T00:00:01Z",
		Tool:      Tool{Name: "Read", Kind: ToolFile},
		Span:      &Span{SemanticType: "file_read", Stage: "started", FilePath: "/a.go", FileOp: "read"},
	}
	p := decodePayload(t, ev)
	if p.EventType != "ActivityStarted" {
		t.Errorf("event_type = %q, want ActivityStarted", p.EventType)
	}
	if p.ActivityID != activityIDFor(ev) {
		t.Errorf("activity_id = %q, want the shared derivation %q", p.ActivityID, activityIDFor(ev))
	}
	if p.ActivityType != "Read" {
		t.Errorf("activity_type = %q, want the tool name", p.ActivityType)
	}
	// workflow_type was absent on the old hook envelope; routing tool events
	// through the typed struct is what fixes that divergence from the base SDK.
	if p.WorkflowType != workflowType {
		t.Errorf("workflow_type = %q, want %q", p.WorkflowType, workflowType)
	}
	in := decodeJSON(t, p.ActivityInput)
	if in["tool_name"] != "Read" || in["kind"] != "file" || in["file_path"] != "/a.go" || in["file_operation"] != "read" {
		t.Errorf("activity_input = %v, want the structural file locators", in)
	}
	// The started half has nothing to report about a call that has not finished.
	if p.ActivityOutput != nil || p.DurationMs != nil {
		t.Errorf("ActivityStarted must not carry output/duration; got %s / %v", p.ActivityOutput, p.DurationMs)
	}
	assertNoSpanKeys(t, decodeRaw(t, ev))
}

// TestBuildPayload_ToolResultIsActivityCompleted holds the completed half: the
// same activity_id, the structural output core runs Guardrails stage "1" over,
// and the duration the dashboard renders — which nothing but the client can
// compute now that there is no span to derive it from.
func TestBuildPayload_ToolResultIsActivityCompleted(t *testing.T) {
	br, lines := 2048, 64
	call := DevEvent{
		EventID: "e1", EventType: EventToolCall, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-08T00:00:01Z",
		Tool:      Tool{Name: "Read", Kind: ToolFile},
		Span:      &Span{SemanticType: "file_read", Stage: "started", FilePath: "/a.go"},
	}
	ev := call
	ev.EventID = "e2"
	ev.EventType = EventToolResult
	ev.Timestamp = "2026-07-08T00:00:02.5Z"
	ev.StartedAt = "2026-07-08T00:00:01Z"
	ev.EndedAt = "2026-07-08T00:00:02.5Z"
	ev.Span = &Span{SemanticType: "file_read", Stage: "completed", FilePath: "/a.go", BytesRead: &br, LinesCount: &lines}

	p := decodePayload(t, ev)
	if p.EventType != "ActivityCompleted" {
		t.Errorf("event_type = %q, want ActivityCompleted", p.EventType)
	}
	// The pair must land on one row, and one approval must address both halves.
	if p.ActivityID != activityIDFor(call) {
		t.Errorf("activity_id = %q, want the started half's %q", p.ActivityID, activityIDFor(call))
	}
	out := decodeJSON(t, p.ActivityOutput)
	if out["bytes_read"] != float64(2048) || out["lines_count"] != float64(64) {
		t.Errorf("activity_output = %v, want the byte/line counts", out)
	}
	if p.DurationMs == nil || *p.DurationMs != 1500 {
		t.Errorf("duration_ms = %v, want 1500", p.DurationMs)
	}
	if p.ActivityInput != nil {
		t.Errorf("input rides the started half only; got %s", p.ActivityInput)
	}
	assertNoSpanKeys(t, decodeRaw(t, ev))
}

// TestDurationMs_OmittedRatherThanZero holds the honest-absence rule. Zero would
// claim a call took no time; the dashboard reads event.duration_ms directly, so
// a fabricated zero is a visible false statement where an absent field is a true
// "unknown".
func TestDurationMs_OmittedRatherThanZero(t *testing.T) {
	base := DevEvent{
		EventID: "e1", EventType: EventToolResult, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-08T00:00:02Z", Tool: Tool{Name: "Bash", Kind: ToolShell},
		Span: &Span{SemanticType: "shell_command", Stage: "completed"},
	}
	for _, tc := range []struct {
		name             string
		startedAt, ended string
	}{
		// The cross-process duration stash missed, so there is no start time.
		{"no start time", "", "2026-07-08T00:00:02Z"},
		{"unparseable start", "not-a-timestamp", "2026-07-08T00:00:02Z"},
		// Clock skew between the two hook processes: end before start is nonsense.
		{"end before start", "2026-07-08T00:00:05Z", "2026-07-08T00:00:02Z"},
		{"identical instants", "2026-07-08T00:00:02Z", "2026-07-08T00:00:02Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := base
			ev.StartedAt = tc.startedAt
			ev.EndedAt = tc.ended
			if got := decodePayload(t, ev).DurationMs; got != nil {
				t.Errorf("duration_ms = %v, want the field omitted", *got)
			}
			if _, present := decodeRaw(t, ev)["duration_ms"]; present {
				t.Error("duration_ms must be absent from the wire, not zero")
			}
		})
	}
}

// TestBuildPayload_MCPActivityInput checks the mcp identifiers reach
// activity_input. They used to ride the span's mcp family fields and its
// mcp.method classifier attribute; with the span gone, activity_input is their
// only home — so an mcp call is still distinguishable from a shell one.
func TestBuildPayload_MCPActivityInput(t *testing.T) {
	ev := DevEvent{
		EventID: "e1", EventType: EventToolCall, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-08T00:00:00Z", Tool: Tool{Name: "mcp__x__y", Kind: ToolMCP, MCPServer: "x"},
		Span: &Span{SemanticType: "mcp_tool_call", Stage: "started", Function: "y", Module: "x"},
	}
	in := decodeJSON(t, decodePayload(t, ev).ActivityInput)
	if in["kind"] != "mcp" || in["mcp_server"] != "x" || in["mcp_tool"] != "y" {
		t.Errorf("activity_input = %v, want kind/mcp_server/mcp_tool", in)
	}
	if in["tool_name"] != "mcp__x__y" {
		t.Errorf("activity_input.tool_name = %v, want the full mcp tool name", in["tool_name"])
	}
}

func TestBuildPayload_EventIDOnWireForIdempotency(t *testing.T) {
	// INV-5: core has no first-class event_id field and does not dedupe dev
	// events, so the idempotency key must ride in metadata to reach the wire.
	ev := DevEvent{
		EventID: "evt-xyz", EventType: EventSessionEnded, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-08T00:00:00Z", Tool: Tool{Name: "session", Kind: ToolShell},
	}
	if m := rawMeta(t, decodePayload(t, ev)); m["event_id"] != "evt-xyz" {
		t.Errorf("metadata.event_id = %v, want evt-xyz", m["event_id"])
	}
}

func TestStripContent_Default_RemovesAllContent(t *testing.T) {
	ev := DevEvent{
		EventID: "e1", EventType: EventToolCall, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-08T00:00:00Z", Tool: Tool{Name: "Edit", Kind: ToolFile},
		Content: &Content{Prompt: "secret prompt", FileText: "file body"},
		Span:    &Span{SemanticType: "file_write", Stage: "started", RequestBody: "diff", ResponseBody: "ok"},
	}
	stripped := stripContent(ev)

	// Original event is never mutated.
	if ev.Content == nil || ev.Span.RequestBody != "diff" {
		t.Error("stripContent mutated the caller's event")
	}
	if stripped.Content != nil {
		t.Error("content not removed")
	}
	if stripped.Span.RequestBody != "" || stripped.Span.ResponseBody != "" {
		t.Error("span bodies not stripped")
	}

	// And nothing content-shaped survives into the wire payload.
	b, _ := buildPayload(stripped)
	if s := string(b); contains(s, "secret prompt") || contains(s, "diff") || contains(s, "file body") {
		t.Errorf("INV-2 violation: content leaked into payload: %s", s)
	}
}

// TestSpanBodiesAreNoLongerAnEgressChannel records a deliberate narrowing. The
// span's request_body/response_body were the one path by which a tool's request
// or response text could reach the wire, and only under content capture. With
// the span retired the serializer does not read them at all, so they cannot
// egress even with capture on.
//
// This removes no working behavior: no adapter has ever populated either field,
// and both adapter mappers have tests asserting they stay empty
// (adapters/claude-code/mapper_test.go:169, adapters/codex/mapper_test.go:207).
// The channel existed and carried nothing.
func TestSpanBodiesAreNoLongerAnEgressChannel(t *testing.T) {
	ev := DevEvent{
		EventID: "e1", EventType: EventToolCall, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-08T00:00:00Z", Tool: Tool{Name: "Edit", Kind: ToolFile},
		// Content capture ON: stripContent has NOT run, so these survive on the
		// event and would reach the wire if anything still read them.
		Span: &Span{SemanticType: "file_write", Stage: "started", RequestBody: "the-diff", ResponseBody: "the-result"},
	}
	b, err := buildPayload(ev)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	if s := string(b); contains(s, "the-diff") || contains(s, "the-result") {
		t.Errorf("span bodies reached the wire: %s", s)
	}
}

// decodeJSON re-parses a json.RawMessage payload field into a map.
func decodeJSON(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		t.Fatal("field is absent; expected an object")
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
