package fakecore

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The wire body is governanceEventPayload, not a DevEvent: a projection whose
// event_type is one of four stored values rather than the client's 35-type
// vocabulary. These predicates are that object's validator, and the contract
// schema is the other object's. Neither is reachable by the other's shape --
// running the DevEvent schema here would reject every event, after which the
// natural fix is to soft-fail it into a no-op.
//
// Restated here rather than imported: a vocabulary the oracle shares with the
// thing it checks can be renamed on both sides at once.
var stockWireTypes = map[string]bool{
	WireWorkflowStarted:   true,
	WireWorkflowCompleted: true,
	WireSignalReceived:    true,
	WireActivityStarted:   true,
	WireActivityCompleted: true,
}

// forbiddenWireKeys must never appear on a delivered body.
//
// spans and span_count: the control plane parses spans[] and then discards it,
// so a span is neither stored nor needed -- and re-adding one is a regression.
// hook_trigger: setting it routes a model turn onto the approval-bypass path,
// so this client never sets it.
var forbiddenWireKeys = []string{"spans", "span_count", "hook_trigger"}

// checkWireShape holds one delivered body to the wire contract. Every check is
// key-level: a captured prompt containing the word "spans" is content, not a
// spans key, and a substring check would refuse it.
func checkWireShape(body map[string]any) []string {
	var reasons []string

	et, _ := body["event_type"].(string)
	if !stockWireTypes[et] {
		reasons = append(reasons, fmt.Sprintf("event_type %q is not one of the stored wire types; the base-wire mapping is broken", et))
	}

	for _, k := range forbiddenWireKeys {
		if _, present := body[k]; present {
			reasons = append(reasons, fmt.Sprintf("body carries a %q key", k))
		}
	}

	for _, k := range []string{"source", "workflow_id", "run_id", "timestamp"} {
		if s, _ := body[k].(string); s == "" {
			reasons = append(reasons, fmt.Sprintf("%s is absent or empty", k))
		}
	}

	// activity_id iff Activity*: it is the approval key and the pairing key,
	// so an Activity row without one cannot be paired or approved, and a
	// non-activity row carrying one would pair against nothing.
	isActivity := et == WireActivityStarted || et == WireActivityCompleted
	if id, _ := body["activity_id"].(string); (id != "") != isActivity {
		if isActivity {
			reasons = append(reasons, "an Activity row carries no activity_id")
		} else {
			reasons = append(reasons, fmt.Sprintf("a %s row carries an activity_id", et))
		}
	}

	// signal_name iff SignalReceived, for the same reason: core requires it on
	// that type and rejects it elsewhere.
	if name, _ := body["signal_name"].(string); (name != "") != (et == WireSignalReceived) {
		if et == WireSignalReceived {
			reasons = append(reasons, "a SignalReceived row carries no signal_name")
		} else {
			reasons = append(reasons, fmt.Sprintf("a %s row carries a signal_name", et))
		}
	}

	reasons = append(reasons, checkModelCallGateShape(body)...)
	return reasons
}

// checkModelCallGateShape holds a model_call_gate row to what separates it from
// a model-call record: it lives in the gate's own activity id namespace, and it
// carries no usage, no cost, no turn index and no reply, in either half. Core
// extracts usage from the literal llm_completion and counts turns by index, so
// any of these on a gate row is the mislabelling this activity type exists to
// prevent. Key-level, like the rest of checkWireShape.
func checkModelCallGateShape(body map[string]any) []string {
	if at, _ := body["activity_type"].(string); at != activityTypeModelCallGate {
		return nil
	}
	var reasons []string
	if id, _ := body["activity_id"].(string); !strings.Contains(id, ":llmgate:") {
		reasons = append(reasons, fmt.Sprintf("a model_call_gate row's activity_id %q is outside the :llmgate: namespace", id))
	}
	for _, field := range []string{"activity_input", "activity_output"} {
		obj, _ := body[field].(map[string]any)
		for _, k := range []string{"usage", "reply_text"} {
			if _, present := obj[k]; present {
				reasons = append(reasons, fmt.Sprintf("a model_call_gate row carries %s.%s", field, k))
			}
		}
	}
	meta, _ := body["metadata"].(map[string]any)
	for _, k := range []string{"tokens", "cost", "turn_index"} {
		if _, present := meta[k]; present {
			reasons = append(reasons, fmt.Sprintf("a model_call_gate row carries metadata.%s", k))
		}
	}
	return reasons
}

// CheckWireShape holds a raw delivered body to the wire contract, for tests
// that want the predicates without a server in front of them.
func CheckWireShape(raw []byte) []string {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return []string{"body is not a JSON object"}
	}
	return checkWireShape(body)
}
