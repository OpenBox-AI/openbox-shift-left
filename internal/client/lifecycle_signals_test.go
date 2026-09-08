package client

import "testing"

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
			t.Errorf("%s: event_type = %q, want %q; a non-accept-listed type is a 400 the "+
				"fail-open path then swallows", tc.et, p.EventType, wireSignalReceived)
		}
		if p.SignalName != tc.name {
			t.Errorf("%s: signal_name = %q, want %q", tc.et, p.SignalName, tc.name)
		}
	}
}

// TestNewSignalsProjectSignalArgs tHE load-bearing case for these three
// events, inverted from v1.8. Their structural detail must reach signal_args,
// which is the only field OPA and Guardrails read on a SignalReceived, AND stay
// in metadata, which the SQL forensics path keys off. The projection duplicates;
// it does not move.
func TestNewSignalsProjectSignalArgs(t *testing.T) {
	for _, et := range []EventType{EventSubagentStarted, EventPermissionDenied, EventAPIError} {
		m := decodeRaw(t, signalEvent(et))
		args, _ := m["signal_args"].(map[string]any)
		if len(args) == 0 {
			t.Errorf("%s carries no signal_args; the class is invisible to every policy engine", et)
			continue
		}
		meta, _ := m["metadata"].(map[string]any)
		if len(meta) == 0 {
			t.Errorf("%s carries no metadata either; the event reports nothing", et)
			continue
		}
		// Projected, not moved: every key the projection emitted is still in
		// metadata, where docs/mapping.md's correlation keys live.
		for k := range args {
			if _, kept := meta[k]; !kept {
				t.Errorf("%s: signal_args[%q] is not in metadata; the projection moved a key "+
					"instead of copying it, and the forensics path reads metadata", et, k)
			}
		}
		for _, k := range []string{"agent_id", "tool_use_id", "error_type"} {
			if _, ok := args[k]; !ok {
				t.Errorf("%s: signal_args is missing structural key %q: %v", et, k, args)
			}
		}
	}
}

// TestPromptSignalStillCarriesItsArgs prompt_submitted is the one signal that
// must keep its args; it is what creates the goal-alignment session in the
// first place.
func TestPromptSignalStillCarriesItsArgs(t *testing.T) {
	ev := signalEvent(EventPromptSubmitted)
	ev.Content = &Content{Prompt: "refactor the spool"}
	p := decodePayload(t, ev)
	if len(p.SignalArgs) == 0 {
		t.Fatal("prompt_submitted lost its signal_args; this is what creates the " +
			"goal-alignment session; without it alignment has no goal at all")
	}
}
