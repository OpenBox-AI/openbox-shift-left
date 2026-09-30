package fakecore

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

const denyVerdict = `{"governance_event_id":"ge","verdict":"block","risk_score":0.9,"action":"stop","fallback_used":false}`

func gateRow(eventType, activityID string, extra map[string]any) Received {
	body := map[string]any{"activity_type": "model_call_gate"}
	for k, v := range extra {
		body[k] = v
	}
	return row(eventType, activityID, "", body)
}

// TestVerdictsByActivityTypeScriptsAGateDenyWithoutTouchingDefault: a model-call
// gate carries no tool_use_id, so the per-call table cannot reach it, and the
// only other lever is Default, which would deny every other event too.
func TestVerdictsByActivityTypeScriptsAGateDenyWithoutTouchingDefault(t *testing.T) {
	s := Script{VerdictsByActivityType: map[string]string{"model_call_gate": denyVerdict}}.withDefaults()

	if _, v := s.answer("", "model_call_gate"); v != denyVerdict {
		t.Errorf("gate verdict = %s, want the scripted deny", v)
	}
	for _, at := range []string{"", "Bash", "llm_completion"} {
		if _, v := s.answer("", at); v != allowVerdict {
			t.Errorf("activity_type %q answered %s; Default must be untouched", at, v)
		}
	}

	// A per-call verdict still wins for a call that has a tool_use_id.
	s.Verdicts = map[string]string{"toolu_1": `{"verdict":"halt"}`}
	if _, v := s.answer("toolu_1", "model_call_gate"); v != `{"verdict":"halt"}` {
		t.Errorf("tool_use_id verdict lost to the activity-type table: %s", v)
	}
}

func TestScenarioCarriesVerdictsByActivityType(t *testing.T) {
	sc := Scenario{VerdictsByActivityType: map[string]string{"model_call_gate": denyVerdict}}
	if sc.Script().VerdictsByActivityType["model_call_gate"] != denyVerdict {
		t.Error("Scenario.Script dropped VerdictsByActivityType")
	}
}

func TestServedGateVerdictIsTheScriptedOne(t *testing.T) {
	f := New(t, Script{VerdictsByActivityType: map[string]string{"model_call_gate": denyVerdict}})
	const base = `"source":"developer-runtime","workflow_id":"w","run_id":"r","timestamp":"2026-09-30T00:00:00Z","event_type":"ActivityStarted"`
	serve := func(body, key string) string {
		req := v3AuthedRequest(t, f, []byte(body))
		req.Header.Set("Idempotency-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	gate := serve(`{`+base+`,"activity_type":"model_call_gate","activity_id":"s:llmgate:r.1"}`, "gate-1")
	other := serve(`{`+base+`,"activity_type":"Bash","activity_id":"a"}`, "other-1")
	if !strings.Contains(gate, `"block"`) {
		t.Errorf("gate answer = %s, want the scripted deny", gate)
	}
	if strings.Contains(other, `"block"`) {
		t.Errorf("a non-gate row got the gate's verdict: %s", other)
	}
}

// TestPairingExcusesAGateFromTheToolUseJoin: a gate has no tool call to identify.
// Denied pre-send it is one started row; allowed, a pair.
func TestPairingExcusesAGateFromTheToolUseJoin(t *testing.T) {
	const id = "s:llmgate:r.1"
	for _, tc := range []struct {
		name    string
		inbox   []Received
		wantBad bool
	}{
		{"a pair", []Received{gateRow(WireActivityStarted, id, nil), gateRow(WireActivityCompleted, id, nil)}, false},
		{"a pre-send deny leaves the started half only", []Received{gateRow(WireActivityStarted, id, nil)}, false},
		{"a completion with no start", []Received{gateRow(WireActivityCompleted, id, nil)}, true},
		{"a duplicated start", []Received{gateRow(WireActivityStarted, id, nil), gateRow(WireActivityStarted, id, nil)}, true},
		{"a duplicated completion", []Received{gateRow(WireActivityStarted, id, nil), gateRow(WireActivityCompleted, id, nil), gateRow(WireActivityCompleted, id, nil)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reasons := checkPairing(Scenario{}, Run{Inbox: tc.inbox})
			if got := len(reasons) > 0; got != tc.wantBad {
				t.Errorf("rejected = %v, want %v: %v", got, tc.wantBad, reasons)
			}
		})
	}
}

// TestWireShapeHoldsAGateToItsContract is the grader the gate's "never a turn,
// never a usage row" rule is checked by, on the delivered bytes.
func TestWireShapeHoldsAGateToItsContract(t *testing.T) {
	const base = `"source":"developer-runtime","workflow_id":"w","run_id":"r","timestamp":"2026-09-30T00:00:00Z","event_type":"ActivityStarted","activity_type":"model_call_gate"`
	for _, tc := range []struct {
		name    string
		body    string
		wantBad bool
	}{
		{"a clean gate row", `{` + base + `,"activity_id":"s:llmgate:r.1","activity_input":{"model":"m"},"metadata":{"provider":"meta"}}`, false},
		{"usage in the output", `{` + base + `,"activity_id":"s:llmgate:r.1","activity_output":{"usage":{"input_tokens":1}}}`, true},
		{"tokens in metadata", `{` + base + `,"activity_id":"s:llmgate:r.1","metadata":{"tokens":{"input":1}}}`, true},
		{"cost in metadata", `{` + base + `,"activity_id":"s:llmgate:r.1","metadata":{"cost":{"amount":1}}}`, true},
		{"a turn index in metadata", `{` + base + `,"activity_id":"s:llmgate:r.1","metadata":{"turn_index":0}}`, true},
		{"not in the gate namespace", `{` + base + `,"activity_id":"s:turn:0"}`, true},
		{"a reply in the output", `{` + base + `,"activity_id":"s:llmgate:r.1","activity_output":{"reply_text":"x"}}`, true},
		{"a turn row naming usage is untouched", `{"source":"developer-runtime","workflow_id":"w","run_id":"r","timestamp":"t","event_type":"ActivityCompleted","activity_type":"llm_completion","activity_id":"s:turn:0","activity_output":{"usage":{"input_tokens":1}},"metadata":{"turn_index":0}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reasons := CheckWireShape([]byte(tc.body))
			if got := len(reasons) > 0; got != tc.wantBad {
				t.Errorf("rejected = %v, want %v: %v", got, tc.wantBad, reasons)
			}
		})
	}
}
