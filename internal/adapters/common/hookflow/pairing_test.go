package hookflow

import (
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// mcpToolCall builds a started (ToolCall) event for an MCP tool the way a
// mapper would: span.operation_id derived from rawArgs, span.invocation_id
// the call's tool_use_id.
func mcpToolCall(sessionID, invocationID, rawArgs, ts string) client.DevEvent {
	return client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       "evt-" + invocationID + "-start-" + ts,
		EventType:     client.EventToolCall,
		SessionID:     sessionID,
		DeveloperDID:  "did:aip:dev",
		Timestamp:     ts,
		StartedAt:     ts,
		Tool:          client.Tool{Name: "mcp__ccd_directory__change_directory", Kind: client.ToolMCP, MCPServer: "ccd_directory"},
		Span: &client.Span{
			SemanticType: "mcp_tool_call",
			Stage:        "started",
			MCPServer:    "ccd_directory",
			Function:     "change_directory",
			InvocationID: invocationID,
			OperationID:  client.OperationForArgs([]byte(rawArgs)),
		},
	}
}

// mcpToolResult builds a completed (ToolResult) event for the same MCP tool.
// rawArgs=="" simulates a completed hook payload carrying no tool_input at
// all, which OperationForArgs also maps to an empty operation id.
func mcpToolResult(sessionID, invocationID, rawArgs, ts string) client.DevEvent {
	var opID string
	if rawArgs != "" {
		opID = client.OperationForArgs([]byte(rawArgs))
	}
	return client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       "evt-" + invocationID + "-end-" + ts,
		EventType:     client.EventToolResult,
		SessionID:     sessionID,
		DeveloperDID:  "did:aip:dev",
		Timestamp:     ts,
		EndedAt:       ts,
		Tool:          client.Tool{Name: "mcp__ccd_directory__change_directory", Kind: client.ToolMCP, MCPServer: "ccd_directory"},
		Span: &client.Span{
			SemanticType: "mcp_tool_call",
			Stage:        "completed",
			MCPServer:    "ccd_directory",
			Function:     "change_directory",
			InvocationID: invocationID,
			OperationID:  opID,
		},
		Status: client.StatusCompleted,
	}
}

// TestRecordDeferred_ThreadsThePairStashEvenWhenTheClosureIsNeverCalled proves
// the stash write happens the moment RecordDeferred runs, not inside the
// closure it returns. The gated enforce path (claude-code/hookrun.go,
// codex/hookrun.go) calls RecordDeferred and invokes that closure only when
// delivery turned out NOT to happen; the common case is a synchronous
// delivery that discards it entirely. If the stash write ever moved into the
// closure, every gated MCP call's duration -- and its Post's adopted operation
// id -- would silently vanish, while observe mode (which always runs the
// closure) kept working, so the gap would hide behind a green test suite.
func TestRecordDeferred_ThreadsThePairStashEvenWhenTheClosureIsNeverCalled(t *testing.T) {
	dir := t.TempDir()
	e := NewEngine(dir)
	const sessionID = "s-mcp-deferred"

	pre := mcpToolCall(sessionID, "tu_1", `{"path":"/a"}`, "2026-09-01T00:00:00Z")
	_ = e.RecordDeferred(pre) // closure discarded: it must never run for this to be a fair test

	post := mcpToolResult(sessionID, "tu_1", `{"path":"/b"}`, "2026-09-01T00:00:05Z")
	if err := e.Record(post); err != nil {
		t.Fatalf("Record(post): %v", err)
	}

	evs := readSpooledEvents(t, dir, sessionID)
	if len(evs) != 1 {
		t.Fatalf("spooled %d events, want 1 (the discarded Pre must never spool)", len(evs))
	}
	spooledPost := evs[0]

	wantActivity := client.ApprovalKeyFor(pre).ActivityID
	if got := client.ApprovalKeyFor(spooledPost).ActivityID; got != wantActivity {
		t.Errorf("spooled Post activity_id = %q, want the Pre's %q; the pair stash must be written "+
			"when RecordDeferred runs, not deferred into the closure it returns", got, wantActivity)
	}
	if spooledPost.StartedAt != pre.StartedAt {
		t.Errorf("spooled Post StartedAt = %q, want the Pre's %q (a real duration)", spooledPost.StartedAt, pre.StartedAt)
	}
}

// TestThreadDuration_MCPPostAdoptsThePresOperationID an MCP call's Pre and
// Post hook processes can map different tool_input (a call site with no
// tool_use_id fallback, or a Post that never receives the arguments at all):
// either way the call's activity_id must not tear in two, and the Post must
// still carry a real duration.
func TestThreadDuration_MCPPostAdoptsThePresOperationID(t *testing.T) {
	for _, tc := range []struct {
		name     string
		postArgs string // "" simulates a completed hook payload with no tool_input
	}{
		{"differing arguments", `{"path":"/b"}`},
		{"no tool_input on the completed half", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			e := NewEngine(dir)
			const sessionID = "s-mcp-adopt"

			pre := mcpToolCall(sessionID, "tu_1", `{"path":"/a"}`, "2026-09-01T00:00:00Z")
			if err := e.Record(pre); err != nil {
				t.Fatalf("Record(pre): %v", err)
			}
			post := mcpToolResult(sessionID, "tu_1", tc.postArgs, "2026-09-01T00:00:05Z")
			if err := e.Record(post); err != nil {
				t.Fatalf("Record(post): %v", err)
			}

			evs := readSpooledEvents(t, dir, sessionID)
			var spooledPre, spooledPost client.DevEvent
			for _, ev := range evs {
				switch ev.EventType {
				case client.EventToolCall:
					spooledPre = ev
				case client.EventToolResult:
					spooledPost = ev
				}
			}

			wantActivity := client.ApprovalKeyFor(spooledPre).ActivityID
			if got := client.ApprovalKeyFor(spooledPost).ActivityID; got != wantActivity {
				t.Errorf("Post activity_id = %q, want the Pre's %q; the call must not tear in two", got, wantActivity)
			}
			if spooledPost.Span.OperationID != spooledPre.Span.OperationID {
				t.Errorf("Post span.operation_id = %q, want the Pre's %q adopted",
					spooledPost.Span.OperationID, spooledPre.Span.OperationID)
			}
			if spooledPost.StartedAt != pre.StartedAt {
				t.Errorf("Post StartedAt = %q, want the Pre's %q (a real duration)", spooledPost.StartedAt, pre.StartedAt)
			}
			if v, _ := spooledPost.Metadata["pair_recovered"].(bool); !v {
				t.Errorf("Post metadata missing pair_recovered:true; the stash changed its operation id, got %v",
					spooledPost.Metadata["pair_recovered"])
			}
		})
	}
}

// TestApprovalRetry_NewInvocationIDSameArgsSharesActivityID pins the fix's
// safety boundary: pairKey's invocation-scoped stash key must never leak into
// activity_id, which stays keyed on the arg-derived operation id alone. A
// retry that mints a fresh invocation id but repeats the same arguments must
// still resolve to the SAME activity_id, or an approved request could never
// be consumed by its retry (claude-code/correlation_test.go
// TestHighRiskClassesHaveAStableOperationID pins the same invariant from the
// mapper side).
func TestApprovalRetry_NewInvocationIDSameArgsSharesActivityID(t *testing.T) {
	dir := t.TempDir()
	e := NewEngine(dir)
	const sessionID = "s-retry"

	original := mcpToolCall(sessionID, "tu_1", `{"title":"x"}`, "2026-09-01T00:00:00Z")
	retry := mcpToolCall(sessionID, "tu_2", `{"title":"x"}`, "2026-09-01T00:01:00Z")

	if err := e.Record(original); err != nil {
		t.Fatalf("Record(original): %v", err)
	}
	if err := e.Record(retry); err != nil {
		t.Fatalf("Record(retry): %v", err)
	}

	evs := readSpooledEvents(t, dir, sessionID)
	if len(evs) != 2 {
		t.Fatalf("spooled %d events, want 2", len(evs))
	}

	want := client.ApprovalKeyFor(evs[0]).ActivityID
	if got := client.ApprovalKeyFor(evs[1]).ActivityID; got != want {
		t.Errorf("retry activity_id = %q, want the original's %q; the pair stash's invocation-scoped "+
			"key must never change what activity_id an approval was filed under", got, want)
	}
}

// TestThreadDuration_RecoversALegacyBareTimestampStash a stash record written
// before the pair format existed is a bare RFC3339 timestamp, no JSON. A
// session that straddles a mid-call upgrade must still recover its duration
// from one, and must never panic trying.
func TestThreadDuration_RecoversALegacyBareTimestampStash(t *testing.T) {
	dir := t.TempDir()
	e := NewEngine(dir)
	const sessionID = "s-legacy"

	post := mcpToolResult(sessionID, "tu_1", `{"path":"/b"}`, "2026-09-01T00:00:05Z")
	if err := e.Durations.putRaw(sessionID, pairKey(post), []byte("2026-09-01T00:00:00Z")); err != nil {
		t.Fatalf("seed legacy stash record: %v", err)
	}

	if err := e.Record(post); err != nil {
		t.Fatalf("Record(post): %v", err)
	}

	evs := readSpooledEvents(t, dir, sessionID)
	if len(evs) != 1 {
		t.Fatalf("spooled %d events, want 1", len(evs))
	}
	if evs[0].StartedAt != "2026-09-01T00:00:00Z" {
		t.Errorf("StartedAt = %q, want the legacy stash's timestamp recovered", evs[0].StartedAt)
	}
	if _, present := evs[0].Metadata["pair_recovered"]; present {
		t.Error("a legacy record carries no operation id, so no adoption happened; " +
			"metadata.pair_recovered must be absent, not false")
	}
}

// TestThreadDuration_StashMissLeavesTheCompletedEventUntouched an unpaired
// completed event (its started half's record was never written, or already
// taken): threading is a no-op beyond what the mapper already put on the
// event, and nothing panics.
func TestThreadDuration_StashMissLeavesTheCompletedEventUntouched(t *testing.T) {
	dir := t.TempDir()
	e := NewEngine(dir)
	const sessionID = "s-miss"

	post := mcpToolResult(sessionID, "tu_never_started", `{"path":"/b"}`, "2026-09-01T00:00:05Z")
	wantOpID := post.Span.OperationID

	if err := e.Record(post); err != nil {
		t.Fatalf("Record(post): %v", err)
	}

	evs := readSpooledEvents(t, dir, sessionID)
	if len(evs) != 1 {
		t.Fatalf("spooled %d events, want 1", len(evs))
	}
	if evs[0].StartedAt != "" {
		t.Errorf("StartedAt = %q, want empty (stash miss keeps today's behavior)", evs[0].StartedAt)
	}
	if evs[0].Span.OperationID != wantOpID {
		t.Errorf("Span.OperationID = %q, want the mapped %q unchanged", evs[0].Span.OperationID, wantOpID)
	}
	if _, present := evs[0].Metadata["pair_recovered"]; present {
		t.Error("a stash miss adopted nothing; metadata.pair_recovered must be absent, not false")
	}
}

// TestThreadDuration_IdenticalArgsCarriesNoPairRecoveredMarker adoption only
// marks the row when it actually changed the operation id; a Post whose own
// arguments matched the Pre's needs no marker, and it must never read false.
func TestThreadDuration_IdenticalArgsCarriesNoPairRecoveredMarker(t *testing.T) {
	dir := t.TempDir()
	e := NewEngine(dir)
	const sessionID = "s-identical"

	pre := mcpToolCall(sessionID, "tu_1", `{"path":"/a"}`, "2026-09-01T00:00:00Z")
	if err := e.Record(pre); err != nil {
		t.Fatalf("Record(pre): %v", err)
	}
	post := mcpToolResult(sessionID, "tu_1", `{"path":"/a"}`, "2026-09-01T00:00:05Z")
	if err := e.Record(post); err != nil {
		t.Fatalf("Record(post): %v", err)
	}

	evs := readSpooledEvents(t, dir, sessionID)
	var spooledPost client.DevEvent
	for _, ev := range evs {
		if ev.EventType == client.EventToolResult {
			spooledPost = ev
		}
	}
	if _, present := spooledPost.Metadata["pair_recovered"]; present {
		t.Errorf("identical arguments changed nothing; metadata.pair_recovered must be absent, got %v",
			spooledPost.Metadata["pair_recovered"])
	}
	if spooledPost.StartedAt != pre.StartedAt {
		t.Errorf("StartedAt = %q, want the Pre's %q even with no divergence", spooledPost.StartedAt, pre.StartedAt)
	}
}

// TestThreadDuration_PairRecoveredMarkerSurvivesNoContentCapture the marker is
// a structural fact ThreadDuration writes after mapping, entirely independent
// of content capture: it never reads ev.Content or any capture flag, so it
// must be set the same way on an event shaped exactly like one content
// capture never touched (nil Content, no content-bearing metadata).
func TestThreadDuration_PairRecoveredMarkerSurvivesNoContentCapture(t *testing.T) {
	dir := t.TempDir()
	e := NewEngine(dir)
	const sessionID = "s-no-capture"

	pre := mcpToolCall(sessionID, "tu_1", `{"path":"/a"}`, "2026-09-01T00:00:00Z")
	if err := e.Record(pre); err != nil {
		t.Fatalf("Record(pre): %v", err)
	}
	post := mcpToolResult(sessionID, "tu_1", `{"path":"/b"}`, "2026-09-01T00:00:05Z")
	post.Content = nil // the shape content-capture-off leaves on every event
	if err := e.Record(post); err != nil {
		t.Fatalf("Record(post): %v", err)
	}

	evs := readSpooledEvents(t, dir, sessionID)
	var spooledPost client.DevEvent
	for _, ev := range evs {
		if ev.EventType == client.EventToolResult {
			spooledPost = ev
		}
	}
	if v, _ := spooledPost.Metadata["pair_recovered"].(bool); !v {
		t.Errorf("metadata.pair_recovered missing with content capture off; want true regardless, got %v",
			spooledPost.Metadata["pair_recovered"])
	}
}

// TestThreadDuration_NilSpanToolResultDoesNotPanic a ToolResult with no Span
// at all must never panic threading its duration.
func TestThreadDuration_NilSpanToolResultDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	e := NewEngine(dir)
	post := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventType:     client.EventToolResult,
		SessionID:     "s-nil-span",
		DeveloperDID:  "did:aip:dev",
		Timestamp:     "2026-09-01T00:00:05Z",
		Tool:          client.Tool{Name: "Bash", Kind: client.ToolShell},
	}
	if err := e.Record(post); err != nil {
		t.Fatalf("Record(post): %v", err)
	}
}
