package client

import "testing"

// The wire `status` field. Its whole value is that core compares it against one
// literal, so these tests are about the two ways it can be wrong without
// looking wrong: a value core scores as a failure, and a value on an event type
// where the column means something else.

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

// A completed tool call reports the literal core reads. Asserted on the decoded
// wire payload rather than on the DevEvent, because the DevEvent field is an
// input and the wire value is the contract.
func TestStatusOnToolResult(t *testing.T) {
	for _, want := range []string{StatusCompleted, StatusFailed} {
		p := decodePayload(t, statusEvent(EventToolResult, want))
		if p.Status != want {
			t.Errorf("status = %q, want %q", p.Status, want)
		}
	}
}

// The exact bytes core compares against. Spelled as literals here, deliberately
// NOT as the constants, so renaming a constant cannot silently rename the wire
// value: this test is the second, independent copy of the contract.
func TestStatusLiteralsMatchTheConsumer(t *testing.T) {
	if StatusCompleted != "completed" {
		t.Errorf("StatusCompleted = %q; openbox-core compares against \"completed\" "+
			"(observability/errors.go:333) and scores every other value as a failure", StatusCompleted)
	}
	if StatusFailed != "failed" {
		t.Errorf("StatusFailed = %q, want \"failed\"", StatusFailed)
	}
}

// A value outside the enum is DROPPED, not forwarded. Core treats anything that
// is not "completed" as a failure, so forwarding "success" would report 0%
// success for calls that all succeeded — indistinguishable from the bug this
// field fixes, but with a plausible-looking payload.
func TestStatusOutsideTheVocabularyIsDropped(t *testing.T) {
	for _, bad := range []string{"success", "COMPLETED", "Completed", "ok", "error", "complete", " completed"} {
		m := decodeRaw(t, statusEvent(EventToolResult, bad))
		if v, present := m["status"]; present {
			t.Errorf("status %q was forwarded as %v; an unrecognized value must be omitted", bad, v)
		}
	}
}

// Scope. payload.status is copied into the row's workflow_status column for ANY
// event type (openbox-core activities/governance/storage_event.go:417), so a
// status on a lifecycle event overwrites a genuinely workflow-scoped field with
// a tool outcome. The client refuses even when an adapter sets it.
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

// Not content (INV-2): a two-literal enum derived from which hook fired cannot
// encode anything, so it is not gated. Emit's stripContent is what runs with
// capture off, and this asserts it leaves status alone — the field must be
// byte-identical in both postures or Tool Health would depend on a privacy
// setting.
func TestStatusSurvivesContentStripping(t *testing.T) {
	ev := statusEvent(EventToolResult, StatusFailed)
	ev.Content = &Content{ToolInput: "rm -rf /tmp/x"}

	stripped := decodeRaw(t, stripContent(ev))
	if stripped["status"] != StatusFailed {
		t.Errorf("status = %v after stripContent, want %q — status is structural, not gated",
			stripped["status"], StatusFailed)
	}
	// And the gated content really was removed, or the assertion above would
	// pass for a payload that stripped nothing at all.
	if in, _ := stripped["activity_input"].(map[string]any); in != nil {
		if _, leaked := in["command"]; leaked {
			t.Errorf("stripContent left the gated command in activity_input: %v", in)
		}
	}
}
