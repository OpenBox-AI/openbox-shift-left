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
	if p.SpanCount != 0 || p.Spans != nil {
		t.Errorf("SessionStarted carries no span; got %+v", p.Spans)
	}
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

func TestBuildPayload_SpanTransportFieldsFilled(t *testing.T) {
	br := 2048
	ev := DevEvent{
		EventID: "e1", EventType: EventToolResult, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-08T00:00:00Z",
		StartedAt: "2026-07-08T00:00:01Z", EndedAt: "2026-07-08T00:00:02Z",
		Tool: Tool{Name: "Read", Kind: ToolFile},
		Span: &Span{SemanticType: "file_read", Stage: "completed", FilePath: "/a.go", BytesRead: &br},
	}
	p := decodePayload(t, ev)
	if len(p.Spans) != 1 {
		t.Fatalf("want 1 span")
	}
	s := p.Spans[0]
	if s.SpanID == "" || s.TraceID == "" {
		t.Error("client must fill span_id/trace_id transport fields")
	}
	// core recomputes semantic_type from the span Name (+ non-nil file_path), so
	// a file_read span must be named "file.read" to be classified correctly.
	if s.Name != "file.read" {
		t.Errorf("span name = %q, want core-recognized file.read", s.Name)
	}
	if s.FilePath == nil || *s.FilePath != "/a.go" {
		t.Errorf("file_path must be set as the classifier gate; got %v", s.FilePath)
	}
	if s.StartTime == 0 || s.EndTime == 0 || s.EndTime <= s.StartTime {
		t.Errorf("start/end epoch-ns not derived: start=%d end=%d", s.StartTime, s.EndTime)
	}
	if s.BytesRead == nil || *s.BytesRead != 2048 {
		t.Errorf("bytes_read = %v, want 2048 (widened to int64)", s.BytesRead)
	}
	// The real tool name is preserved in metadata (Name is repurposed for core).
	if m := rawMeta(t, p); m["tool_name"] != "Read" {
		t.Errorf("metadata.tool_name = %v, want Read", m["tool_name"])
	}
}

func TestBuildPayload_MCPSpanClassificationAttribute(t *testing.T) {
	ev := DevEvent{
		EventID: "e1", EventType: EventToolCall, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-08T00:00:00Z", Tool: Tool{Name: "mcp__x__y", Kind: ToolMCP, MCPServer: "x"},
		Span: &Span{SemanticType: "mcp_tool_call", Stage: "started", Function: "y", Module: "x"},
	}
	s := decodePayload(t, ev).Spans[0]
	// core classifies MCP only via attributes["mcp.method"]; hook_type/function
	// are ignored. The client must set the attribute for mcp_tool_call to land.
	if s.Attributes == nil || s.Attributes["mcp.method"] != "callTool" {
		t.Errorf(`attributes["mcp.method"] = %v, want callTool`, s.Attributes)
	}
	if s.Name != "mcp__x__y" {
		t.Errorf("mcp span name = %q, want tool name", s.Name)
	}
	if s.FuncName == nil || *s.FuncName != "y" {
		t.Errorf(`function tag mismap: %v`, s.FuncName)
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

func TestContentGate_Enabled_CarriesSpanBodies(t *testing.T) {
	// When content-capture is enabled, Emit does NOT strip, so span bodies reach
	// the payload. (buildPayload is post-gate; here we call it directly to prove
	// the carry-through.)
	ev := DevEvent{
		EventID: "e1", EventType: EventToolCall, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-08T00:00:00Z", Tool: Tool{Name: "Edit", Kind: ToolFile},
		Span: &Span{SemanticType: "file_write", Stage: "started", RequestBody: "the-diff"},
	}
	s := decodePayload(t, ev).Spans[0]
	if s.RequestBody == nil || *s.RequestBody != "the-diff" {
		t.Errorf("enabled content must carry span request_body; got %v", s.RequestBody)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
