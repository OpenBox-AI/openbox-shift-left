package conformance

import (
	"encoding/json"
	"strings"
	"testing"
)

// A turn event names its producer with exactly one discriminator field, and the
// nested oneOf on both turn branches is what enforces it.
//
// This is the invariant every model-call lane rests on. Five producers describe
// the same kind of thing — a model turn — and client/payload.go derives a
// disjoint activity_id per producer from these fields. Core's dedupe key includes
// activity_id, so two producers that could mint one id would have half their
// evidence absorbed as a duplicate with no error anywhere. Two discriminators on
// ONE event is the same failure from the other direction: the derivation would
// silently pick whichever branch it reached first, and the event would be
// attributed to a producer that did not observe it.
//
// The contract mistake this guards has shipped once already: v1.5 required
// turn_index unconditionally, so every gateway event failed its own schema
// (ADR-0021 records it). v1.6 adds two producers and repairs a third, which is
// three more chances to make it.

// turnDiscriminators is every field that names a turn's producer. A new lane adds
// its field here and to the schema in the same change, or the cases below stop
// covering it.
var turnDiscriminators = map[string]any{
	"turn_index":         0,
	"gateway_request_id": "gw-1a2b3c",
	"session_rollup":     true,
	"otel_request_id":    "req_011CSxKq9mNp",
	"proxy_request_id":   "px-4f2a9c1e7b30",
}

// turnBase is a turn event with NO discriminator. Each case adds its own.
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

// Each producer's own discriminator, alone, validates — on BOTH halves of the
// pair. TurnStarted is included deliberately: it carried the unconditional
// turn_index requirement, so a repair applied only to TurnCompleted would leave
// every new lane's opening event failing its own contract.
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

// Two discriminators on one event is rejected — every pair, both halves.
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

// A turn event naming no producer is rejected. Without this, an event whose
// discriminator failed to be set validates and the client mints no activity_id
// for it, so the pair never correlates onto one row.
func TestNoDiscriminatorRejected(t *testing.T) {
	for _, et := range []string{"TurnStarted", "TurnCompleted"} {
		if err := ValidateDevEvent(marshalEvent(t, turnBase(et)), false); err == nil {
			t.Errorf("%s with no discriminator: want rejection, got nil", et)
		}
	}
}

// `session_rollup: false` must NOT satisfy the rollup branch.
//
// The branch is presence-based (`required`) but the client's derivation is
// truthiness-based (`if ev.SessionRollup`), and those disagree on exactly one
// value. An event carrying `false` and no other discriminator would validate
// while `turnActivityIDFor` returned "" — a contract-valid turn the client
// cannot mint an activity_id for, so the pair never correlates onto a row.
//
// Unreachable from THIS client (the field is `omitempty`, so a false bool is
// never marshalled), but the contract is adapter-facing and an adapter is not
// obliged to be Go. The schema is the half that moves: making the client
// presence-based instead would change when `<session>:usage:rollup` is minted.
func TestRollupFalseIsNotADiscriminator(t *testing.T) {
	for _, et := range []string{"TurnStarted", "TurnCompleted"} {
		ev := turnBase(et)
		ev["session_rollup"] = false
		if err := ValidateDevEvent(marshalEvent(t, ev), false); err == nil {
			t.Errorf("%s with session_rollup:false alone: want rejection, got nil — "+
				"the client would derive no activity_id for this event", et)
		}

		// But `false` beside a real discriminator is just a hook turn, which is
		// what Go's false≡absent means at the derivation. Rejecting it would
		// reject events an adapter may legitimately marshal.
		ok := turnBase(et)
		ok["session_rollup"] = false
		ok["turn_index"] = 0
		if err := ValidateDevEvent(marshalEvent(t, ok), false); err != nil {
			t.Errorf("%s with turn_index and session_rollup:false: want valid, got %v", et, err)
		}
	}
}

// The two new ids are upstream-controlled text that reaches a stored key
// verbatim, so the contract states their bound rather than leaving it to each
// producer (gatewayemit.usableRequestID is the precedent this mirrors).
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

		// The ACCEPT side of the same boundary. Without it an off-by-one that
		// started rejecting a legal 128-character id would ship unnoticed: every
		// case above still passes, and no fixture anywhere comes near the ceiling.
		// A bound is two claims, and only one of them is tested by rejections.
		ev := turnBase("TurnCompleted")
		ev[field] = strings.Repeat("x", 128)
		if err := ValidateDevEvent(marshalEvent(t, ev), false); err != nil {
			t.Errorf("%s at exactly the 128-character bound: want valid, got %v", field, err)
		}
	}
}

// turnDiscriminators above is a hand-maintained list, and a hand-maintained list
// that only a comment binds to the schema drifts. This asserts it IS the schema's
// list: exactly the union of `required` names across $defs.turnProducer's branches.
//
// Without it, a sixth lane added to the schema alone leaves every case above
// silently not covering it — the test suite stays green and proves less than it
// did, which is the failure mode this contract has already shipped twice.
func TestDiscriminatorListMatchesTheSchema(t *testing.T) {
	schema, err := LoadSchema()
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	defs, _ := schema["$defs"].(map[string]any)
	producer, _ := defs["turnProducer"].(map[string]any)
	branches, _ := producer["oneOf"].([]any)
	if len(branches) == 0 {
		t.Fatal("$defs.turnProducer has no oneOf branches — the exactly-one rule is gone")
	}

	inSchema := map[string]bool{}
	for _, b := range branches {
		m, _ := b.(map[string]any)
		req, _ := m["required"].([]any)
		if len(req) != 1 {
			t.Errorf("branch %q requires %d fields, want exactly 1 — a branch requiring two "+
				"discriminators, or none, is not a producer", m["title"], len(req))
		}
		for _, r := range req {
			s, _ := r.(string)
			inSchema[s] = true
		}
	}

	for name := range inSchema {
		if _, ok := turnDiscriminators[name]; !ok {
			t.Errorf("schema declares producer %q but turnDiscriminators does not — "+
				"every case in this file silently skips it", name)
		}
	}
	for name := range turnDiscriminators {
		if !inSchema[name] {
			t.Errorf("turnDiscriminators has %q but no schema branch requires it — "+
				"the tests assert a producer the contract does not have", name)
		}
	}
}
