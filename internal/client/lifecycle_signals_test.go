package client

import "testing"

// The three failure/lifecycle signals (ADR-0018): SubagentStarted,
// PermissionDenied, APIError. All ride stock SignalReceived (INV-8).

func signalEvent(t EventType) DevEvent {
	return DevEvent{
		SchemaVersion: SchemaVersion,
		EventID:       "ev-signal",
		EventType:     t,
		SessionID:     "sess-signal",
		DeveloperDID:  "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		Timestamp:     "2026-08-13T10:00:00Z",
		Tool:          Tool{Name: "claude-code", Kind: ToolShell},
		Metadata: map[string]any{
			"agent_id": "agt-1", "agent_type": "code-reviewer",
			"tool_use_id": "toolu_1", "error_type": "rate_limit",
		},
	}
}

func TestNewSignalsMapToStockWireTypes(t *testing.T) {
	for _, tc := range []struct {
		et   EventType
		name string
	}{
		{EventSubagentStarted, "subagent_started"},
		{EventPermissionDenied, "permission_denied"},
		{EventAPIError, "api_error"},
	} {
		p := decodePayload(t, signalEvent(tc.et))
		if p.EventType != wireSignalReceived {
			t.Errorf("%s: event_type = %q, want %q — a non-accept-listed type is a 400 the "+
				"fail-open path then swallows", tc.et, p.EventType, wireSignalReceived)
		}
		if p.SignalName != tc.name {
			t.Errorf("%s: signal_name = %q, want %q", tc.et, p.SignalName, tc.name)
		}
	}
}

// THE load-bearing case for these three events.
//
// openbox-core's alignment engine treats ANY SignalReceived carrying non-empty
// signal_args as a new user goal: it scores the assistant messages accumulated
// so far against the previous goal and then OVERWRITES the session's goal with
// the stringified args (internal/services/age.go:112-137).
//
// So a plausible, well-intentioned change — "surface the denied tool in the
// Verify tab's Input, which reads signal_args" — would replace the developer's
// actual prompt with a metadata blob as the thing every later turn is scored
// against. Goal alignment would not error; it would quietly start measuring
// drift from "permission_denied". Structural detail rides metadata instead, and
// this test is what keeps it there.
func TestNewSignalsCarryNoSignalArgs(t *testing.T) {
	for _, et := range []EventType{EventSubagentStarted, EventPermissionDenied, EventAPIError} {
		m := decodeRaw(t, signalEvent(et))
		if v, present := m["signal_args"]; present {
			t.Errorf("%s carries signal_args = %v; core would read that as a NEW USER GOAL "+
				"and overwrite the alignment session's goal with it (age.go:112-137). "+
				"Structural detail belongs in metadata", et, v)
		}
		// And the detail really is carried somewhere, or this test would pass
		// for an event that reports nothing at all.
		meta, _ := m["metadata"].(map[string]any)
		if len(meta) == 0 {
			t.Errorf("%s carries no metadata either; the event reports nothing", et)
		}
	}
}

// prompt_submitted is the one signal that MUST keep its args — it is what
// creates the goal-alignment session in the first place. Asserted here beside
// the negative case so the distinction is visible in one place rather than
// inferred from two files.
func TestPromptSignalStillCarriesItsArgs(t *testing.T) {
	ev := signalEvent(EventPromptSubmitted)
	ev.Content = &Content{Prompt: "refactor the spool"}
	p := decodePayload(t, ev)
	if len(p.SignalArgs) == 0 {
		t.Fatal("prompt_submitted lost its signal_args — this is what creates the " +
			"goal-alignment session; without it alignment has no goal at all")
	}
}
