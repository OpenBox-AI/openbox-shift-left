package client

import (
	"bytes"
	"encoding/json"
	"testing"
)

// This file is the E7-S4 wire-shape conformance + pairing suite for the
// ToolCall/ToolResult → ActivityStarted+hook mapping. It builds real payloads
// with buildPayload and asserts them against the E7-S1 conformance gate
// (AssertHookWireShape) plus the started/completed pairing contract, so the
// event→wire mapping is proven to emit exactly what the base SDK's
// assert_hook_wire_shape accepts.

// decodeHookPayload builds a tool DevEvent's payload and re-parses it as the
// decoded map Core (and AssertHookWireShape) sees on the wire.
func decodeHookPayload(t *testing.T, ev DevEvent) map[string]any {
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

// firstSpan returns the single decoded span map from a hook payload.
func firstSpan(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	spans, ok := p["spans"].([]any)
	if !ok || len(spans) != 1 {
		t.Fatalf("want exactly 1 span, got %v", p["spans"])
	}
	s, ok := spans[0].(map[string]any)
	if !ok {
		t.Fatalf("span not an object: %v", spans[0])
	}
	return s
}

func fileToolEvent(et EventType, stage string) DevEvent {
	br := 12
	return DevEvent{
		EventID: "e-" + stage, EventType: et, SessionID: "sess-1", DeveloperDID: "did:aip:abc",
		Timestamp: "2026-07-08T00:00:00Z", StartedAt: "2026-07-08T00:00:00Z", EndedAt: "2026-07-08T00:00:02Z",
		Tool: Tool{Name: "Read", Kind: ToolFile},
		Span: &Span{SemanticType: "file_read", Stage: stage, FilePath: "/a.go", FileOp: "read", BytesRead: &br},
	}
}

// TestHookPayload_ConformsToBaseWireShape drives the E7-S1 conformance gate over
// file / mcp / shell tool calls in BOTH stages — the Go analog of the base SDK's
// test_started_and_completed_wire_shape (every captured payload must satisfy the
// one assertion, and completed payloads are ActivityStarted too).
func TestHookPayload_ConformsToBaseWireShape(t *testing.T) {
	cases := map[string]DevEvent{
		"file/started": fileToolEvent(EventToolCall, "started"),
		"file/completed": fileToolEvent(EventToolResult, "completed"),
		"mcp/started": {
			EventID: "m1", EventType: EventToolCall, SessionID: "sess-1", DeveloperDID: "did:aip:abc",
			Timestamp: "2026-07-08T00:00:00Z", StartedAt: "2026-07-08T00:00:00Z",
			Tool: Tool{Name: "mcp__srv__do", Kind: ToolMCP, MCPServer: "srv"},
			Span: &Span{SemanticType: "mcp_tool_call", Stage: "started", MCPServer: "srv", Function: "do"},
		},
		"shell/completed": {
			EventID: "s1", EventType: EventToolResult, SessionID: "sess-1", DeveloperDID: "did:aip:abc",
			Timestamp: "2026-07-08T00:00:00Z", EndedAt: "2026-07-08T00:00:02Z",
			Tool: Tool{Name: "Bash", Kind: ToolShell},
			Span: &Span{SemanticType: "internal", Stage: "completed"},
		},
	}
	for name, ev := range cases {
		t.Run(name, func(t *testing.T) {
			p := decodeHookPayload(t, ev)
			if err := AssertHookWireShape(p); err != nil {
				t.Fatalf("payload violates base hook wire shape: %v", err)
			}
			// Both stages serialize as ActivityStarted; the span stage is the only
			// started-vs-completed distinguisher (base SDK ground truth).
			if p["event_type"] != "ActivityStarted" {
				t.Errorf("event_type = %v, want ActivityStarted (completed is NOT ActivityCompleted)", p["event_type"])
			}
			s := firstSpan(t, p)
			wantStage := "started"
			if ev.EventType == EventToolResult {
				wantStage = "completed"
			}
			if s["stage"] != wantStage {
				t.Errorf("span stage = %v, want %v", s["stage"], wantStage)
			}
		})
	}
}

// TestHookPayload_Envelope asserts the session-attach + hook envelope fields Core
// needs sit alongside the spans on the flat top-level dict.
func TestHookPayload_Envelope(t *testing.T) {
	ev := fileToolEvent(EventToolCall, "started")
	ev.WorkspaceID = "repo-x"
	p := decodeHookPayload(t, ev)

	if p["source"] != "developer-runtime" {
		t.Errorf("source = %v", p["source"])
	}
	if p["workflow_id"] != "repo-x" || p["run_id"] != "sess-1" {
		t.Errorf("(workflow_id, run_id) = (%v,%v)", p["workflow_id"], p["run_id"])
	}
	if p["timestamp"] != "2026-07-08T00:00:00Z" {
		t.Errorf("timestamp = %v", p["timestamp"])
	}
	if p["hook_trigger"] != true {
		t.Errorf("hook_trigger = %v, want true", p["hook_trigger"])
	}
	// activity_type is the tool name (the dashboard Activity label).
	if p["activity_type"] != "Read" {
		t.Errorf("activity_type = %v, want Read", p["activity_type"])
	}
	// metadata is a nested object carrying the idempotency key + real tool name.
	m, ok := p["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata not a nested object: %T", p["metadata"])
	}
	if m["event_id"] != "e-started" || m["tool_name"] != "Read" {
		t.Errorf("metadata = %v, want event_id + tool_name", m)
	}
}

// TestHookPayload_FileClassificationFields asserts a file ToolCall carries the
// Core file classifier's source fields at the span root (name + file_path), not
// an SDK-computed semantic_type.
func TestHookPayload_FileClassificationFields(t *testing.T) {
	s := firstSpan(t, decodeHookPayload(t, fileToolEvent(EventToolCall, "started")))
	if s["hook_type"] != "file_operation" {
		t.Errorf("hook_type = %v, want file_operation", s["hook_type"])
	}
	if s["name"] != "file.read" {
		t.Errorf("name = %v, want file.read (Core's file classifier gate)", s["name"])
	}
	if s["file_path"] != "/a.go" {
		t.Errorf("file_path = %v, want /a.go", s["file_path"])
	}
	if s["file_operation"] != "read" {
		t.Errorf("file_operation = %v, want read", s["file_operation"])
	}
	if _, present := s["semantic_type"]; present {
		t.Error("semantic_type must NOT be sent (Core computes it)")
	}
	// The whole file family tuple is present even when the caller supplied only
	// some of it (present-but-null — required by AssertHookWireShape).
	for _, f := range FamilyRootFields[HookFileOperation] {
		if _, present := s[f]; !present {
			t.Errorf("missing file family field %q", f)
		}
	}
}

// TestHookPayload_MCPClassificationFields asserts the mcp hook carries the
// attribute Core reads today (mcp.method) plus the structural mcp identifiers.
func TestHookPayload_MCPClassificationFields(t *testing.T) {
	ev := DevEvent{
		EventID: "m1", EventType: EventToolCall, SessionID: "sess-1", DeveloperDID: "did:aip:abc",
		Timestamp: "2026-07-08T00:00:00Z", StartedAt: "2026-07-08T00:00:00Z",
		Tool: Tool{Name: "mcp__srv__do", Kind: ToolMCP, MCPServer: "srv"},
		Span: &Span{SemanticType: "mcp_tool_call", Stage: "started", MCPServer: "srv", Function: "do"},
	}
	s := firstSpan(t, decodeHookPayload(t, ev))
	if s["hook_type"] != "mcp" {
		t.Errorf("hook_type = %v, want mcp", s["hook_type"])
	}
	attrs, _ := s["attributes"].(map[string]any)
	if attrs["mcp.method"] != "callTool" {
		t.Errorf(`attributes["mcp.method"] = %v, want callTool (Core's mcp gate)`, attrs["mcp.method"])
	}
	if attrs["mcp.server"] != "srv" || attrs["mcp.tool"] != "do" {
		t.Errorf("mcp.server/mcp.tool attrs = %v", attrs)
	}
	if s["mcp_server"] != "srv" || s["mcp_tool"] != "do" || s["mcp_method"] != "callTool" {
		t.Errorf("mcp family root fields mismap: server=%v tool=%v method=%v", s["mcp_server"], s["mcp_tool"], s["mcp_method"])
	}
	// kind is CLIENT for mcp (leaves the process), INTERNAL otherwise.
	if s["kind"] != "CLIENT" {
		t.Errorf("mcp kind = %v, want CLIENT", s["kind"])
	}
}

// TestHookPayload_ShellHookType asserts a shell tool maps to hook_type=shell with
// no command content on the observe path (INV-2 — shell_command stays null).
func TestHookPayload_ShellHookType(t *testing.T) {
	ev := DevEvent{
		EventID: "s1", EventType: EventToolCall, SessionID: "sess-1", DeveloperDID: "did:aip:abc",
		Timestamp: "2026-07-08T00:00:00Z", StartedAt: "2026-07-08T00:00:00Z",
		Tool: Tool{Name: "Bash", Kind: ToolShell},
		Span: &Span{SemanticType: "internal", Stage: "started"},
	}
	s := firstSpan(t, decodeHookPayload(t, ev))
	if s["hook_type"] != "shell" {
		t.Errorf("hook_type = %v, want shell", s["hook_type"])
	}
	if s["kind"] != "INTERNAL" {
		t.Errorf("shell kind = %v, want INTERNAL", s["kind"])
	}
	// shell_command is present (family tuple) but null — the command is never on
	// the observe/egress path (it is read only for the local enforce decision).
	if v, present := s["shell_command"]; !present || v != nil {
		t.Errorf("shell_command = %v (present=%v), want present-but-null (INV-2)", v, present)
	}
}

// TestHookPayload_StartedCompletedPairing is the core E7-S4 contract: the
// ToolCall (started) and ToolResult (completed) of the SAME tool call share a
// trace_id, span_id, and activity_id (so Core/the dashboard pair them onto one
// timeline row), while the started span nulls end_time/duration_ns and the
// completed span fills them.
func TestHookPayload_StartedCompletedPairing(t *testing.T) {
	started := decodeHookPayload(t, fileToolEvent(EventToolCall, "started"))
	completed := decodeHookPayload(t, fileToolEvent(EventToolResult, "completed"))

	if started["activity_id"] == "" || started["activity_id"] != completed["activity_id"] {
		t.Errorf("activity_id must pair: started=%v completed=%v", started["activity_id"], completed["activity_id"])
	}
	sp, cp := firstSpan(t, started), firstSpan(t, completed)
	if sp["span_id"] != cp["span_id"] {
		t.Errorf("span_id must be shared across the pair: %v vs %v", sp["span_id"], cp["span_id"])
	}
	if sp["trace_id"] != cp["trace_id"] {
		t.Errorf("trace_id must be shared across the session: %v vs %v", sp["trace_id"], cp["trace_id"])
	}
	// Started nulls end_time/duration_ns; completed fills them.
	if sp["end_time"] != nil || sp["duration_ns"] != nil {
		t.Errorf("started span must null end_time/duration_ns: end=%v dur=%v", sp["end_time"], sp["duration_ns"])
	}
	if cp["end_time"] == nil || cp["duration_ns"] == nil {
		t.Errorf("completed span must fill end_time/duration_ns: end=%v dur=%v", cp["end_time"], cp["duration_ns"])
	}
}

// TestHookPayload_TraceAndActivityScoping asserts the derived ids are scoped
// correctly: same session ⇒ shared trace_id; distinct tool calls ⇒ distinct
// activity_id/span_id; distinct sessions ⇒ distinct trace_id.
func TestHookPayload_TraceAndActivityScoping(t *testing.T) {
	readA := fileToolEvent(EventToolCall, "started")
	readB := fileToolEvent(EventToolCall, "started")
	readB.Span.FilePath = "/b.go" // a different tool call in the same session

	pa, pb := decodeHookPayload(t, readA), decodeHookPayload(t, readB)
	if firstSpan(t, pa)["trace_id"] != firstSpan(t, pb)["trace_id"] {
		t.Error("same session must share trace_id")
	}
	if pa["activity_id"] == pb["activity_id"] {
		t.Error("distinct tool calls must have distinct activity_id")
	}
	if firstSpan(t, pa)["span_id"] == firstSpan(t, pb)["span_id"] {
		t.Error("distinct tool calls must have distinct span_id")
	}

	otherSession := fileToolEvent(EventToolCall, "started")
	otherSession.SessionID = "sess-2"
	if firstSpan(t, pa)["trace_id"] == firstSpan(t, decodeHookPayload(t, otherSession))["trace_id"] {
		t.Error("distinct sessions must have distinct trace_id")
	}
}

// TestHookPayload_ContentGate asserts INV-2 on the flat hook path: a stripped
// tool call leaks no content into the payload bytes, and an authorized
// (content-capture-on) call carries the gated bodies at the span root only.
func TestHookPayload_ContentGate(t *testing.T) {
	// Stripped (default): Emit has already nulled the bodies, so nothing content-
	// shaped reaches the wire.
	stripped := stripContent(DevEvent{
		EventID: "e1", EventType: EventToolCall, SessionID: "sess-1", DeveloperDID: "did:aip:abc",
		Timestamp: "2026-07-08T00:00:00Z", Tool: Tool{Name: "Edit", Kind: ToolFile},
		Content: &Content{Prompt: "secret prompt", FileText: "file body"},
		Span:    &Span{SemanticType: "file_write", Stage: "started", FilePath: "/x.go", RequestBody: "the-diff", ResponseBody: "resp"},
	})
	b, err := buildPayload(stripped)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	for _, leak := range []string{"secret prompt", "file body", "the-diff", "resp"} {
		if contains(string(b), leak) {
			t.Errorf("INV-2 violation: %q leaked into payload: %s", leak, b)
		}
	}

	// Authorized (content-capture on ⇒ Emit does not strip): the gated bodies ride
	// at the span root (never in attributes/metadata).
	authorized := DevEvent{
		EventID: "e2", EventType: EventToolCall, SessionID: "sess-1", DeveloperDID: "did:aip:abc",
		Timestamp: "2026-07-08T00:00:00Z", Tool: Tool{Name: "Edit", Kind: ToolFile},
		Span: &Span{SemanticType: "file_write", Stage: "started", FilePath: "/x.go", RequestBody: "the-diff"},
	}
	s := firstSpan(t, decodeHookPayload(t, authorized))
	if s["request_body"] != "the-diff" {
		t.Errorf("authorized content must carry request_body at the span root; got %v", s["request_body"])
	}
	attrs, _ := s["attributes"].(map[string]any)
	if len(attrs) != 0 {
		t.Errorf("attributes must carry no content; got %v", attrs)
	}
}

// TestHookPayload_ContentBodyTruncated asserts G_SEC SEC-1: an authorized
// (content-capture-on) body that exceeds maxBodySize is size-capped before egress
// (the base SDK's max_body_size privacy cap, mirrored), so the opt-in path cannot
// ship an unbounded body. The cap is applied before marshal ⇒ before signing.
func TestHookPayload_ContentBodyTruncated(t *testing.T) {
	huge := make([]rune, maxBodySize+5000)
	for i := range huge {
		huge[i] = 'x'
	}
	ev := DevEvent{
		EventID: "e1", EventType: EventToolCall, SessionID: "sess-1", DeveloperDID: "did:aip:abc",
		Timestamp: "2026-07-08T00:00:00Z", Tool: Tool{Name: "Edit", Kind: ToolFile},
		Span: &Span{SemanticType: "file_write", Stage: "started", FilePath: "/x.go", RequestBody: string(huge)},
	}
	s := firstSpan(t, decodeHookPayload(t, ev))
	body, _ := s["request_body"].(string)
	if len([]rune(body)) != maxBodySize {
		t.Errorf("request_body length = %d runes, want capped to %d", len([]rune(body)), maxBodySize)
	}
}

// TestHookPayload_RealPostShape_StatelessDuration exercises the ToolResult shape
// with ONLY EndedAt set (StartedAt empty). Since E7-S8 the adapter threads the
// PreToolUse start time onto the completed event, so this is now the stash-MISS
// FALLBACK (an unpaired PostToolUse, or a lost started record), NOT the normal
// path — see TestHookPayload_ThreadedStart_RealDuration for the threaded case. It
// documents F1: the completed span still conforms and pairs with its ToolCall
// (shared span_id), but with no start time its duration_ns is 0 and its
// start_time is its own timestamp — a known limitation of the miss path, not a
// wire break.
func TestHookPayload_RealPostShape_StatelessDuration(t *testing.T) {
	// The started half (PreToolUse: StartedAt set, EndedAt empty).
	started := DevEvent{
		EventID: "pre", EventType: EventToolCall, SessionID: "sess-1", DeveloperDID: "did:aip:abc",
		Timestamp: "2026-07-08T00:00:00Z", StartedAt: "2026-07-08T00:00:00Z",
		Tool: Tool{Name: "Read", Kind: ToolFile},
		Span: &Span{SemanticType: "file_read", Stage: "started", FilePath: "/a.go", FileOp: "read"},
	}
	// The completed half (PostToolUse: ONLY EndedAt set — the real mapper shape).
	completed := DevEvent{
		EventID: "post", EventType: EventToolResult, SessionID: "sess-1", DeveloperDID: "did:aip:abc",
		Timestamp: "2026-07-08T00:00:02Z", EndedAt: "2026-07-08T00:00:02Z",
		Tool: Tool{Name: "Read", Kind: ToolFile},
		Span: &Span{SemanticType: "file_read", Stage: "completed", FilePath: "/a.go", FileOp: "read"},
	}

	cp := decodeHookPayload(t, completed)
	if err := AssertHookWireShape(cp); err != nil {
		t.Fatalf("real PostToolUse payload violates wire shape: %v", err)
	}
	cs := firstSpan(t, cp)
	if cs["stage"] != "completed" || cs["end_time"] == nil {
		t.Errorf("completed span: stage=%v end_time=%v", cs["stage"], cs["end_time"])
	}
	// Documented F1 limitation: stateless completed span ⇒ duration_ns 0.
	if d, _ := cs["duration_ns"].(float64); d != 0 {
		t.Errorf("duration_ns = %v; the stateless PostToolUse shape yields 0 (documented)", cs["duration_ns"])
	}
	// Pairing still holds across the real (asymmetric-timing) pair.
	sp := firstSpan(t, decodeHookPayload(t, started))
	if sp["span_id"] != cs["span_id"] {
		t.Errorf("real pair must still share span_id: %v vs %v", sp["span_id"], cs["span_id"])
	}
}

// TestHookPayload_ThreadedStart_RealDuration is the E7-S8 payoff: once the adapter
// threads the PreToolUse start time onto the completed ToolResult (StartedAt set),
// buildHookSpan computes a REAL duration_ns = end-start on the completed span and
// its start_time equals the started span's — so core's completed-hook path lands a
// non-zero event-level duration_ms. It also proves the pair shares span_id and
// start_time, exactly as the base SDK's single-span-object pairing does.
func TestHookPayload_ThreadedStart_RealDuration(t *testing.T) {
	started := DevEvent{
		EventID: "pre", EventType: EventToolCall, SessionID: "sess-1", DeveloperDID: "did:aip:abc",
		Timestamp: "2026-07-08T00:00:00Z", StartedAt: "2026-07-08T00:00:00Z",
		Tool: Tool{Name: "Read", Kind: ToolFile},
		Span: &Span{SemanticType: "file_read", Stage: "started", FilePath: "/a.go", FileOp: "read"},
	}
	// The threaded completed half: StartedAt recovered from the paired PreToolUse
	// (durationStash), EndedAt from PostToolUse — a 2s tool call.
	completed := DevEvent{
		EventID: "post", EventType: EventToolResult, SessionID: "sess-1", DeveloperDID: "did:aip:abc",
		Timestamp: "2026-07-08T00:00:02Z", StartedAt: "2026-07-08T00:00:00Z", EndedAt: "2026-07-08T00:00:02Z",
		Tool: Tool{Name: "Read", Kind: ToolFile},
		Span: &Span{SemanticType: "file_read", Stage: "completed", FilePath: "/a.go", FileOp: "read"},
	}

	cp := decodeHookPayload(t, completed)
	if err := AssertHookWireShape(cp); err != nil {
		t.Fatalf("threaded completed payload violates wire shape: %v", err)
	}
	cs := firstSpan(t, cp)
	sp := firstSpan(t, decodeHookPayload(t, started))

	// Real duration: 2s = 2e9 ns.
	if d, _ := cs["duration_ns"].(float64); d != 2e9 {
		t.Errorf("duration_ns = %v, want 2e9 (2s) from the threaded start", cs["duration_ns"])
	}
	// start_time shared with the started span (both derived from the same start).
	if cs["start_time"] != sp["start_time"] {
		t.Errorf("threaded completed start_time %v must equal started %v", cs["start_time"], sp["start_time"])
	}
	if cs["span_id"] != sp["span_id"] {
		t.Errorf("pair must share span_id: %v vs %v", cs["span_id"], sp["span_id"])
	}
}

// TestHookPayload_SpanFunctionIsNotWireDataForNonMCP pins the guarantee both
// adapters rely on to use span.function as a local pairing channel: for shell,
// file, and generic tool hook types the field feeds only the derived
// activity_id/span_id and must never appear in the emitted payload. Only the
// MCP branch reads it as wire data (mcp.tool / mcp_tool). If this ever changes,
// the provider tool_use_id — an identifier, but one we promise not to egress on
// observe events — starts leaking, so fail loudly here rather than in review.
func TestHookPayload_SpanFunctionIsNotWireDataForNonMCP(t *testing.T) {
	const marker = "toolu_pairing_marker"

	shell := DevEvent{
		EventID: "e-shell", EventType: EventToolCall, SessionID: "sess-1", DeveloperDID: "did:aip:abc",
		Timestamp: "2026-07-08T00:00:00Z", StartedAt: "2026-07-08T00:00:00Z",
		Tool: Tool{Name: "Bash", Kind: ToolShell},
		Span: &Span{SemanticType: "shell_command", Stage: "started", Function: marker},
	}
	file := fileToolEvent(EventToolCall, "started")
	file.Span.Function = marker

	for name, ev := range map[string]DevEvent{"shell": shell, "file": file} {
		b, err := buildPayload(ev)
		if err != nil {
			t.Fatalf("%s: buildPayload: %v", name, err)
		}
		if bytes.Contains(b, []byte(marker)) {
			t.Errorf("%s: span.function leaked to the wire: %s", name, b)
		}
	}

	// The MCP branch is the deliberate exception — there it is wire data.
	mcp := DevEvent{
		EventID: "e-mcp", EventType: EventToolCall, SessionID: "sess-1", DeveloperDID: "did:aip:abc",
		Timestamp: "2026-07-08T00:00:00Z", StartedAt: "2026-07-08T00:00:00Z",
		Tool: Tool{Name: "mcp__srv__do", Kind: ToolMCP, MCPServer: "srv"},
		Span: &Span{SemanticType: "mcp_tool_call", Stage: "started", MCPServer: "srv", Function: "do"},
	}
	b, err := buildPayload(mcp)
	if err != nil {
		t.Fatalf("mcp: buildPayload: %v", err)
	}
	if !bytes.Contains(b, []byte(`"mcp_tool":"do"`)) && !bytes.Contains(b, []byte(`"mcp.tool":"do"`)) {
		t.Errorf("mcp: function should be wire data for MCP, payload: %s", b)
	}
}
