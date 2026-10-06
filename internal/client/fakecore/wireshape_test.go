package fakecore

import "testing"

// TestWireShapeChecksKeysNotSubstrings is the distinction the predicate set
// exists to make. Content is not structure: a developer whose prompt discusses
// spans must not be refused, and a body that actually carries a spans key must
// be, because core parses that key and discards it and re-adding one is a
// regression.
func TestWireShapeChecksKeysNotSubstrings(t *testing.T) {
	const base = `"source":"developer-runtime","workflow_id":"w","run_id":"r","timestamp":"2026-09-14T00:00:00Z"`

	for _, tc := range []struct {
		name    string
		body    string
		wantBad bool
	}{
		{"a well-formed activity row", `{` + base + `,"event_type":"ActivityStarted","activity_id":"a"}`, false},
		{"a prompt that talks about spans", `{` + base + `,"event_type":"SignalReceived","signal_name":"prompt_submitted","signal_args":{"goal":"explain how spans and span_count differ"}}`, false},
		{"a body carrying a spans key", `{` + base + `,"event_type":"ActivityStarted","activity_id":"a","spans":[]}`, true},
		{"a body carrying span_count", `{` + base + `,"event_type":"ActivityStarted","activity_id":"a","span_count":0}`, true},
		{"a body carrying hook_trigger", `{` + base + `,"event_type":"ActivityStarted","activity_id":"a","hook_trigger":"x"}`, true},
		{"an activity row with no activity_id", `{` + base + `,"event_type":"ActivityStarted"}`, true},
		{"a signal row carrying an activity_id", `{` + base + `,"event_type":"SignalReceived","signal_name":"n","activity_id":"a"}`, true},
		{"a signal row with no signal_name", `{` + base + `,"event_type":"SignalReceived"}`, true},
		{"an activity row carrying a signal_name", `{` + base + `,"event_type":"ActivityStarted","activity_id":"a","signal_name":"n"}`, true},
		{"a DevEvent type on the wire", `{` + base + `,"event_type":"tool_call"}`, true},
		{"a row with no run_id", `{"source":"developer-runtime","workflow_id":"w","timestamp":"t","event_type":"WorkflowStarted"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reasons := CheckWireShape([]byte(tc.body))
			if got := len(reasons) > 0; got != tc.wantBad {
				t.Errorf("rejected = %v, want %v; reasons: %v", got, tc.wantBad, reasons)
			}
		})
	}
}
