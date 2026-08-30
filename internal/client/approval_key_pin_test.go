package client

import "testing"

// Change any derivation and every in-flight approval becomes unaddressable;
// the grant an approver made can never be consumed, the retry files a fresh
// request, and a rewake that says "re-run to proceed" loops.

// pinEvent is the fixed event the pinned ids are derived from.
func pinEvent() DevEvent {
	return DevEvent{
		SchemaVersion: SchemaVersion,
		EventID:       "ev-pin-1",
		EventType:     EventToolCall,
		SessionID:     "sess-pin-0001",
		DeveloperDID:  "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		WorkspaceID:   "openbox-ai/openbox-shift-left",
		Timestamp:     "2026-07-31T09:00:00Z",
		Tool:          Tool{Name: "Write", Kind: ToolFile},
		Span: &Span{
			SemanticType: "file_write",
			Stage:        "started",
			// It does not track the real tree and must not be "corrected" when a
			// directory moves; a rename sweep already did that once and broke the byte
			// pin, which is core's dedupe key.
			FilePath:     "cli/cmd/openbox/main.go",
			FileOp:       "write",
			InvocationID: "toolu_pin_attempt_1",
			OperationID:  "op-pin-write-main",
		},
	}
}

const (
	pinWorkflowID    = "openbox-ai/openbox-shift-left"
	pinRunID         = "sess-pin-0001"
	pinActivityID    = "cc-act-e490dad4315c494b702ce1978a4e114b"
	pinDIDWorkflowID = "did:aip:7f3c9b2e-0000-5000-a000-000000000001"
)

// TestApprovalKeyIsPinned holds the exact wire ids for a fixed event.
func TestApprovalKeyIsPinned(t *testing.T) {
	ev := pinEvent()

	if got := workflowIDFor(ev); got != pinWorkflowID {
		t.Errorf("workflow id = %q, want %q", got, pinWorkflowID)
	}
	if got := activityIDFor(ev); got != pinActivityID {
		t.Errorf("activity id = %q, want %q", got, pinActivityID)
	}

	k := ApprovalKeyFor(ev)
	if k.WorkflowID != pinWorkflowID {
		t.Errorf("ApprovalKey.WorkflowID = %q, want %q", k.WorkflowID, pinWorkflowID)
	}
	if k.RunID != pinRunID {
		t.Errorf("ApprovalKey.RunID = %q, want %q", k.RunID, pinRunID)
	}
	if k.ActivityID != pinActivityID {
		t.Errorf("ApprovalKey.ActivityID = %q, want %q", k.ActivityID, pinActivityID)
	}
	if !k.Valid() {
		t.Error("ApprovalKey.Valid() = false; a key that cannot address a record is a broken hold")
	}
}

// TestApprovalKeyWorkflowIDFallsBackToDID pins the fallback branch. An adapter
// that leaves WorkspaceID empty (the Claude Code mapper does, deliberately)
// must still produce a stable workflow_id, or (workflow_id, run_id) fragments
// and the poll addresses nothing.
func TestApprovalKeyWorkflowIDFallsBackToDID(t *testing.T) {
	ev := pinEvent()
	ev.WorkspaceID = ""

	if got := workflowIDFor(ev); got != pinDIDWorkflowID {
		t.Errorf("workflow id = %q, want %q", got, pinDIDWorkflowID)
	}
	if got := ApprovalKeyFor(ev).WorkflowID; got != pinDIDWorkflowID {
		t.Errorf("ApprovalKeyFor.WorkflowID = %q, want %q", got, pinDIDWorkflowID)
	}
}

// TestApprovalKeyIsStableAcrossStagesAndRetries pins the two invariants that
// make an approval consumable: the started and completed halves of one call
// address one record, and so does a retry of the same operation.
func TestApprovalKeyIsStableAcrossStagesAndRetries(t *testing.T) {
	want := ApprovalKeyFor(pinEvent())

	result := pinEvent()
	result.EventID = "ev-pin-2"
	result.EventType = EventToolResult
	result.Timestamp = "2026-07-31T09:00:02.5Z"
	result.StartedAt = "2026-07-31T09:00:00Z"
	result.EndedAt = "2026-07-31T09:00:02.5Z"
	span := *result.Span
	span.Stage = "completed"
	result.Span = &span
	if got := ApprovalKeyFor(result); got != want {
		t.Errorf("completed half key = %+v, want %+v (started and completed must address one record)", got, want)
	}

	retry := pinEvent()
	retry.EventID = "ev-pin-3"
	retry.Timestamp = "2026-07-31T09:05:00Z"
	rspan := *retry.Span
	rspan.InvocationID = "toolu_pin_attempt_2"
	retry.Span = &rspan
	if got := ApprovalKeyFor(retry); got != want {
		t.Errorf("retry key = %+v, want %+v (a retry must consume the approval already granted)", got, want)
	}

	other := pinEvent()
	ospan := *other.Span
	ospan.OperationID = "op-pin-write-other"
	other.Span = &ospan
	if got := ApprovalKeyFor(other); got == want {
		t.Errorf("distinct operations share activity_id %q; one approval would release both", got.ActivityID)
	}
}
