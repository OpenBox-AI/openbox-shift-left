package client

import "testing"

func statusEvent(t EventType, status string) DevEvent {
	ev := DevEvent{
		SchemaVersion: SchemaVersion,
		EventID:       "ev-status",
		EventType:     t,
		SessionID:     "sess-status",
		DeveloperDID:  "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		Timestamp:     "2026-08-13T10:00:00Z",
		Tool:          Tool{Name: "Bash", Kind: ToolShell},
		Status:        status,
	}
	switch t {
	case EventToolCall, EventToolResult:
		ev.Span = &Span{SemanticType: "internal", Stage: "completed"}
	case EventTurnStarted, EventTurnCompleted:
		idx := 0
		ev.TurnIndex = &idx
	}
	return ev
}

// TestStatusOnToolResult a completed tool call reports the literal core reads.
func TestStatusOnToolResult(t *testing.T) {
	for _, want := range []string{StatusCompleted, StatusFailed} {
		p := decodePayload(t, statusEvent(EventToolResult, want))
		if p.Status != want {
			t.Errorf("status = %q, want %q", p.Status, want)
		}
	}
}

// TestStatusLiteralsMatchTheConsumer the exact bytes core compares against.
// Spelled as literals here, deliberately NOT as the constants, so renaming a
// constant cannot silently rename the wire value: this test is the second,
// independent copy of the contract.
func TestStatusLiteralsMatchTheConsumer(t *testing.T) {
	if StatusCompleted != "completed" {
		t.Errorf("StatusCompleted = %q; openbox-core compares against \"completed\" "+
			"(observability/errors.go:333) and scores every other value as a failure", StatusCompleted)
	}
	if StatusFailed != "failed" {
		t.Errorf("StatusFailed = %q, want \"failed\"", StatusFailed)
	}
}

// TestStatusOutsideTheVocabularyIsDropped a value outside the enum is dropped,
// not forwarded.
func TestStatusOutsideTheVocabularyIsDropped(t *testing.T) {
	for _, bad := range []string{"success", "COMPLETED", "Completed", "ok", "error", "complete", " completed"} {
		m := decodeRaw(t, statusEvent(EventToolResult, bad))
		if v, present := m["status"]; present {
			t.Errorf("status %q was forwarded as %v; an unrecognized value must be omitted", bad, v)
		}
	}
}

// TestStatusRidesToolResultsOnly scope. Payload.status is copied into the
// row's workflow_status column for ANY event type (openbox-core
// activities/governance/storage_event.go:417), so a status on a lifecycle
// event overwrites a genuinely workflow-scoped field with a tool outcome.
func TestStatusRidesToolResultsOnly(t *testing.T) {
	for _, et := range []EventType{
		EventSessionStarted, EventSessionEnded, EventPromptSubmitted, EventDeploy,
		EventToolCall, EventTurnStarted, EventTurnCompleted,
	} {
		m := decodeRaw(t, statusEvent(et, StatusCompleted))
		if v, present := m["status"]; present {
			t.Errorf("%s carries status = %v; the field is ToolResult-only", et, v)
		}
	}
}

// TestStatusSurvivesContentStripping not content (INV-2): a two-literal enum
// derived from which hook fired cannot encode anything, so it is not gated.
func TestStatusSurvivesContentStripping(t *testing.T) {
	ev := statusEvent(EventToolResult, StatusFailed)
	ev.Content = &Content{ToolInput: "rm -rf /tmp/x"}

	stripped := decodeRaw(t, stripContent(ev))
	if stripped["status"] != StatusFailed {
		t.Errorf("status = %v after stripContent, want %q; status is structural, not gated",
			stripped["status"], StatusFailed)
	}
	if in, _ := stripped["activity_input"].(map[string]any); in != nil {
		if _, leaked := in["command"]; leaked {
			t.Errorf("stripContent left the gated command in activity_input: %v", in)
		}
	}
}
