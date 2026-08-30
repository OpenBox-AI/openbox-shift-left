package conformance

import (
	"encoding/json"
	"strings"
	"testing"
)

// Two discriminators on ONE event is the same failure from the other
// direction: the derivation would silently pick whichever branch it reached
// first, and the event would be attributed to a producer that did not observe
// it.

var turnDiscriminators = map[string]any{
	"turn_index":         0,
	"gateway_request_id": "gw-1a2b3c",
	"session_rollup":     true,
	"otel_request_id":    "req_011CSxKq9mNp",
	"proxy_request_id":   "px-4f2a9c1e7b30",
}

func turnBase(eventType string) map[string]any {
	return map[string]any{
		"schema_version":     "1.6",
		"event_id":           "evt-disc-1",
		"event_type":         eventType,
		"openbox_session_id": "sess-abc123",
		"developer_did":      "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		"timestamp":          "2026-08-28T09:20:11Z",
		"tool":               map[string]any{"name": "claude-code", "kind": "shell"},
	}
}

func marshalEvent(t *testing.T, m map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// TestOneDiscriminatorValidates each producer's own discriminator, alone,
// validates; on both halves of the pair.
func TestOneDiscriminatorValidates(t *testing.T) {
	for _, et := range []string{"TurnStarted", "TurnCompleted"} {
		for field, value := range turnDiscriminators {
			ev := turnBase(et)
			ev[field] = value
			if err := ValidateDevEvent(marshalEvent(t, ev), false); err != nil {
				t.Errorf("%s with %s alone: want valid, got %v", et, field, err)
			}
		}
	}
}

// TestTwoDiscriminatorsRejected two discriminators on one event is rejected;
// every pair, both halves.
func TestTwoDiscriminatorsRejected(t *testing.T) {
	fields := make([]string, 0, len(turnDiscriminators))
	for f := range turnDiscriminators {
		fields = append(fields, f)
	}
	for _, et := range []string{"TurnStarted", "TurnCompleted"} {
		for i := 0; i < len(fields); i++ {
			for j := i + 1; j < len(fields); j++ {
				ev := turnBase(et)
				ev[fields[i]] = turnDiscriminators[fields[i]]
				ev[fields[j]] = turnDiscriminators[fields[j]]
				if err := ValidateDevEvent(marshalEvent(t, ev), false); err == nil {
					t.Errorf("%s with both %s and %s: want rejection, got nil", et, fields[i], fields[j])
				}
			}
		}
	}
}

// TestNoDiscriminatorRejected a turn event naming no producer is rejected.
// Without this, an event whose discriminator failed to be set validates and
// the client mints no activity_id for it, so the pair never correlates onto
// one row.
func TestNoDiscriminatorRejected(t *testing.T) {
	for _, et := range []string{"TurnStarted", "TurnCompleted"} {
		if err := ValidateDevEvent(marshalEvent(t, turnBase(et)), false); err == nil {
			t.Errorf("%s with no discriminator: want rejection, got nil", et)
		}
	}
}

// `session_rollup: false` must NOT satisfy the rollup branch. An event
// carrying `false` and no other discriminator would validate while
// `turnActivityIDFor` returned ""; a contract-valid turn the client cannot
// mint an activity_id for, so the pair never correlates onto a row.
func TestRollupFalseIsNotADiscriminator(t *testing.T) {
	for _, et := range []string{"TurnStarted", "TurnCompleted"} {
		ev := turnBase(et)
		ev["session_rollup"] = false
		if err := ValidateDevEvent(marshalEvent(t, ev), false); err == nil {
			t.Errorf("%s with session_rollup:false alone: want rejection, got nil; "+
				"the client would derive no activity_id for this event", et)
		}

		ok := turnBase(et)
		ok["session_rollup"] = false
		ok["turn_index"] = 0
		if err := ValidateDevEvent(marshalEvent(t, ok), false); err != nil {
			t.Errorf("%s with turn_index and session_rollup:false: want valid, got %v", et, err)
		}
	}
}

// TestNewRequestIDsAreBounded the two new ids are upstream-controlled text
// that reaches a stored key verbatim, so the contract states their bound
// rather than leaving it to each producer (gatewayemit.usableRequestID is the
// precedent this mirrors).
func TestNewRequestIDsAreBounded(t *testing.T) {
	cases := map[string]string{
		"oversized": strings.Repeat("x", 129),
		"newline":   "req\n123",
		"space":     "req 123",
		"control":   "req\x01123",
		"empty":     "",
		"non-ascii": "req_ü123",
	}
	for _, field := range []string{"otel_request_id", "proxy_request_id"} {
		for name, bad := range cases {
			ev := turnBase("TurnCompleted")
			ev[field] = bad
			if err := ValidateDevEvent(marshalEvent(t, ev), false); err == nil {
				t.Errorf("%s = %q (%s): want rejection, got nil", field, bad, name)
			}
		}

		ev := turnBase("TurnCompleted")
		ev[field] = strings.Repeat("x", 128)
		if err := ValidateDevEvent(marshalEvent(t, ev), false); err != nil {
			t.Errorf("%s at exactly the 128-character bound: want valid, got %v", field, err)
		}
	}
}

// TestDiscriminatorListMatchesTheSchema turnDiscriminators above is a hand-
// maintained list, and a hand-maintained list that only a comment binds to the
// schema drifts.
func TestDiscriminatorListMatchesTheSchema(t *testing.T) {
	schema, err := LoadSchema()
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	defs, _ := schema["$defs"].(map[string]any)
	producer, _ := defs["turnProducer"].(map[string]any)
	branches, _ := producer["oneOf"].([]any)
	if len(branches) == 0 {
		t.Fatal("$defs.turnProducer has no oneOf branches; the exactly-one rule is gone")
	}

	inSchema := map[string]bool{}
	for _, b := range branches {
		m, _ := b.(map[string]any)
		req, _ := m["required"].([]any)
		if len(req) != 1 {
			t.Errorf("branch %q requires %d fields, want exactly 1; a branch requiring two "+
				"discriminators, or none, is not a producer", m["title"], len(req))
		}
		for _, r := range req {
			s, _ := r.(string)
			inSchema[s] = true
		}
	}

	for name := range inSchema {
		if _, ok := turnDiscriminators[name]; !ok {
			t.Errorf("schema declares producer %q but turnDiscriminators does not; "+
				"every case in this file silently skips it", name)
		}
	}
	for name := range turnDiscriminators {
		if !inSchema[name] {
			t.Errorf("turnDiscriminators has %q but no schema branch requires it; "+
				"the tests assert a producer the contract does not have", name)
		}
	}
}
