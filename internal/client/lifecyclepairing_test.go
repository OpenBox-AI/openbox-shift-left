package client

import (
	"encoding/json"
	"testing"
)

// This is the one assertion that would have caught every defect in this repair.
// The live suite DOES assert pairing (test/20-capture.sh), but filters every
// query to TOOL activity types, so the llm_completion rows sat outside it.
//
// The invariant: every activity_id carries exactly two rows, one ActivityStarted
// and one ActivityCompleted; Workflow* are one each; SignalReceived is the only
// unpaired type. So total = W + 2A + S, with W = 1 for a session still in flight.
//
// Three ways to get it wrong: `total % 2` is even only when W + S is; a parity
// COUNT is fooled by two compensating errors; and an in-flight session is
// legitimately short one event. Hence the per-activity_id form.

// wireEvent is the shape of the assertion's input: what actually goes on the wire.
type wireEvent struct {
	EventType  string `json:"event_type"`
	ActivityID string `json:"activity_id"`
	SignalName string `json:"signal_name"`
}

func wireEventOf(t *testing.T, ev DevEvent) wireEvent {
	t.Helper()
	raw, err := buildPayload(ev)
	if err != nil {
		t.Fatalf("buildPayload(%s): %v", ev.EventType, err)
	}
	var w wireEvent
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	return w
}

// assertLifecyclePairing is the check itself, so every session shape below is
// held to identical rules.
func assertLifecyclePairing(t *testing.T, name string, events []wireEvent, ended bool) {
	t.Helper()

	perActivity := map[string]map[string]int{}
	counts := map[string]int{}
	for _, e := range events {
		counts[e.EventType]++
		switch e.EventType {
		case wireActivityStarted, wireActivityCompleted:
			if e.ActivityID == "" {
				t.Errorf("%s: an %s carries no activity_id, so nothing can pair it", name, e.EventType)
				continue
			}
			if perActivity[e.ActivityID] == nil {
				perActivity[e.ActivityID] = map[string]int{}
			}
			perActivity[e.ActivityID][e.EventType]++
		case wireSignalReceived:
			if e.SignalName == "" {
				t.Errorf("%s: a SignalReceived carries no signal_name", name)
			}
		}
	}

	// The check. Zero offenders required.
	for id, halves := range perActivity {
		total := halves[wireActivityStarted] + halves[wireActivityCompleted]
		if total != 2 || halves[wireActivityStarted] != 1 || halves[wireActivityCompleted] != 1 {
			t.Errorf("%s: activity_id %q has %d Started and %d Completed, want exactly one of each. "+
				"The dashboard timeline pairs the two, so a single-sided activity is a contract "+
				"violation rather than a documentable choice.",
				name, id, halves[wireActivityStarted], halves[wireActivityCompleted])
		}
	}

	wantWorkflow := 1
	if ended {
		wantWorkflow = 2
	}
	gotWorkflow := counts[wireWorkflowStarted] + counts[wireWorkflowCompleted]
	if gotWorkflow != wantWorkflow {
		t.Errorf("%s: %d workflow event(s), want %d (an in-flight session has WorkflowStarted and "+
			"no WorkflowCompleted yet, which is why W is not always 2)", name, gotWorkflow, wantWorkflow)
	}

	// total = W + 2A + S, which is the arithmetic the per-activity check above
	// already implies; asserting it too catches an event type that grew a new wire
	// mapping without anyone deciding whether it pairs.
	wantTotal := gotWorkflow + 2*len(perActivity) + counts[wireSignalReceived]
	if len(events) != wantTotal {
		t.Errorf("%s: %d events, but W + 2A + S = %d. Some event type is neither a paired activity, "+
			"a workflow boundary, nor a signal.", name, len(events), wantTotal)
	}
}

// TestEveryEventTypeIsEitherPairedAWorkflowBoundaryOrASignal is the census: the
// population is fixed by wireTypeFor, so a new event type cannot quietly join it
// without a decision about which of the three it is.
func TestEveryEventTypeIsEitherPairedAWorkflowBoundaryOrASignal(t *testing.T) {
	starts := map[EventType]bool{EventToolCall: true, EventTurnStarted: true}
	completes := map[EventType]bool{EventToolResult: true, EventTurnCompleted: true}

	for _, et := range AllEventTypes {
		wire, signal, err := wireTypeFor(et)
		if err != nil {
			t.Errorf("%s has no wire mapping at all", et)
			continue
		}
		switch wire {
		case wireActivityStarted:
			if !starts[et] {
				t.Errorf("%s maps to ActivityStarted but is not in this test's opening set; decide "+
					"which half it is and what pairs with it", et)
			}
		case wireActivityCompleted:
			if !completes[et] {
				t.Errorf("%s maps to ActivityCompleted but nothing is declared to open it", et)
			}
		case wireSignalReceived:
			if signal == "" {
				t.Errorf("%s maps to SignalReceived with no signal_name", et)
			}
		case wireWorkflowStarted, wireWorkflowCompleted:
		default:
			t.Errorf("%s maps to the unknown wire type %q", et, wire)
		}
	}
}

// TestASyntheticSessionSatisfiesTheLifecyclePairingInvariant covers every event
// type a session can contain, and every model-call producer, in one pass.
func TestASyntheticSessionSatisfiesTheLifecyclePairingInvariant(t *testing.T) {
	const session = "sess-pairing"
	const did = "did:aip:7f3c9b2e-0000-5000-a000-000000000001"

	base := func(et EventType) DevEvent {
		return DevEvent{
			SchemaVersion: SchemaVersion,
			EventID:       "ev-" + string(et),
			EventType:     et,
			SessionID:     session,
			DeveloperDID:  did,
			Timestamp:     "2026-09-01T10:00:12Z",
			StartedAt:     "2026-09-01T10:00:00Z",
			EndedAt:       "2026-09-01T10:00:12Z",
			Tool:          Tool{Name: "claude-code", Kind: ToolShell},
		}
	}
	toolPair := func(name, path string) []DevEvent {
		call := base(EventToolCall)
		call.Tool = Tool{Name: name, Kind: ToolFile}
		call.Span = &Span{SemanticType: "file_write", Stage: "started", FilePath: path, OperationID: path}
		result := base(EventToolResult)
		result.Tool = call.Tool
		result.Span = &Span{SemanticType: "file_write", Stage: "completed", FilePath: path, OperationID: path}
		result.Status = StatusCompleted
		return []DevEvent{call, result}
	}
	turnPair := func(mutate func(*DevEvent)) []DevEvent {
		started, completed := base(EventTurnStarted), base(EventTurnCompleted)
		mutate(&started)
		mutate(&completed)
		return []DevEvent{started, completed}
	}
	idx := func(n int) *int { return &n }

	var evs []DevEvent
	evs = append(evs, base(EventSessionStarted))
	evs = append(evs, base(EventPromptSubmitted))
	evs = append(evs, toolPair("Write", "/tmp/a.go")...)
	evs = append(evs, toolPair("Edit", "/tmp/b.go")...)
	// A hook-lane turn.
	evs = append(evs, turnPair(func(e *DevEvent) { e.TurnIndex = idx(0); e.Model = "claude-opus-4-8" })...)
	// A subagent's turn, whose activity_id is partitioned so it cannot collide.
	evs = append(evs, turnPair(func(e *DevEvent) { e.TurnIndex = idx(0); e.AgentID = "agent-7" })...)
	// One turn from each in-path lane, and one from the telemetry lane.
	evs = append(evs, turnPair(func(e *DevEvent) { e.ProxyRequestID = "px-1" })...)
	evs = append(evs, turnPair(func(e *DevEvent) { e.GatewayRequestID = "gw-1" })...)
	evs = append(evs, turnPair(func(e *DevEvent) { e.OtelRequestID = "otel-1" })...)
	// Codex's session-wide usage rollup.
	evs = append(evs, turnPair(func(e *DevEvent) { e.SessionRollup = true })...)
	evs = append(evs, base(EventSubagentStarted), base(EventPermissionDenied), base(EventAPIError))
	evs = append(evs, base(EventSessionEnded))

	wire := make([]wireEvent, 0, len(evs))
	for _, ev := range evs {
		wire = append(wire, wireEventOf(t, ev))
	}
	assertLifecyclePairing(t, "synthetic session", wire, true)

	// The reference session's arithmetic, restated against this one so the shape is
	// not merely internally consistent: 62 events = 1 + 2(26) + 9 there, with 26
	// activity pairs and zero unpaired. Here: W=2, A=8 (two tool calls plus six
	// turns -- hook, subagent, proxy, gateway, otel, rollup), S=4.
	const wantW, wantA, wantS = 2, 8, 4
	if got := len(wire); got != wantW+2*wantA+wantS {
		t.Errorf("the fixture is %d events; W + 2A + S = %d + 2(%d) + %d = %d",
			got, wantW, wantA, wantS, wantW+2*wantA+wantS)
	}
}

// TestAnInFlightSessionIsNotAFailure is the caution that produces a wrong
// conclusion: a live session is legitimately short its WorkflowCompleted, and a
// naive check false-alarms on every running session.
func TestAnInFlightSessionIsNotAFailure(t *testing.T) {
	base := DevEvent{
		SchemaVersion: SchemaVersion,
		EventID:       "ev-1",
		SessionID:     "sess-live",
		DeveloperDID:  "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		Timestamp:     "2026-09-01T10:00:12Z",
		StartedAt:     "2026-09-01T10:00:00Z",
		EndedAt:       "2026-09-01T10:00:12Z",
		Tool:          Tool{Name: "claude-code", Kind: ToolShell},
	}
	started, completed := base, base
	started.EventType, completed.EventType = EventTurnStarted, EventTurnCompleted
	started.ProxyRequestID, completed.ProxyRequestID = "px-live", "px-live"
	open := base
	open.EventType = EventSessionStarted
	prompt := base
	prompt.EventType = EventPromptSubmitted

	var wire []wireEvent
	for _, ev := range []DevEvent{open, prompt, started, completed} {
		wire = append(wire, wireEventOf(t, ev))
	}
	// Four events: W=1, A=1, S=1. Total is even here, but that is incidental --
	// the per-activity_id form is what makes this pass for the right reason.
	assertLifecyclePairing(t, "in-flight session", wire, false)
}

// TestASingleSidedActivityFailsTheCheck the check has to be able to fail, or
// every assertion above is decoration. This is the exact defect this repair
// closed: an ActivityCompleted with no ActivityStarted.
func TestASingleSidedActivityFailsTheCheck(t *testing.T) {
	probe := &testing.T{}
	assertLifecyclePairing(probe, "broken", []wireEvent{
		{EventType: wireWorkflowStarted},
		{EventType: wireWorkflowCompleted},
		{EventType: wireActivityCompleted, ActivityID: "sess:proxy:px-1"},
	}, true)
	if !probe.Failed() {
		t.Error("an ActivityCompleted with no ActivityStarted passed the pairing check")
	}
}

// TestTwoCompensatingErrorsFailTheCheck a parity count is fooled by exactly this:
// one missing Started plus one orphan Started nets to even. The per-activity_id
// form is not.
func TestTwoCompensatingErrorsFailTheCheck(t *testing.T) {
	probe := &testing.T{}
	assertLifecyclePairing(probe, "compensating", []wireEvent{
		{EventType: wireWorkflowStarted},
		{EventType: wireWorkflowCompleted},
		{EventType: wireActivityCompleted, ActivityID: "sess:proxy:px-1"}, // missing its Started
		{EventType: wireActivityStarted, ActivityID: "sess:proxy:px-2"},   // orphan
	}, true)
	if !probe.Failed() {
		t.Error("two compensating errors netted to even and passed; this is why parity is not the check")
	}
}
