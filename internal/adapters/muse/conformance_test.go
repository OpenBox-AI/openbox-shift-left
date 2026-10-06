package muse

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/conformance"
)

// Every event the adapter produces must validate against the dev-event schema,
// with capture off (no content may be present) and with capture on. If the
// contract tightens, this breaks here rather than silently at ingest.
func TestEmittedEventsAreConformant(t *testing.T) {
	secretFree := func(s string) string { return s }
	for _, capture := range []bool{false, true} {
		m := testMapper()
		m.CaptureContent = capture
		m.RedactContent = secretFree
		for name, want := range fixtureEvents {
			if want == "" {
				continue
			}
			ev := parseFixture(t, name)
			got, ok := m.Map(hookOf(ev), ev)
			if !ok {
				t.Errorf("%s: did not map", name)
				continue
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if err := conformance.ValidateDevEvent(raw, capture); err != nil {
				t.Errorf("%s (capture=%v) is not dev-event schema conformant:\n%s\nerror: %v", name, capture, raw, err)
			}
			if strings.Contains(string(raw), `"spans"`) {
				t.Errorf("%s carries spans[]", name)
			}
		}
	}
}

// The gate events are what this adapter adds to the contract, so they are also
// held to their own rules: the discriminator, the pair's shared id, and the
// fields the schema forbids.
func TestModelCallGateEventsValidateAndPair(t *testing.T) {
	m := testMapper()
	m.CaptureContent = true
	pre, _ := m.Map(HookPreLLMCall, parseFixture(t, "pre-llm-call"))
	post, _ := m.Map(HookPostLLMCall, parseFixture(t, "post-llm-call"))
	for _, ev := range []client.DevEvent{pre, post} {
		raw, _ := json.Marshal(ev)
		if err := conformance.ValidateDevEvent(raw, true); err != nil {
			t.Errorf("%s: %v\n%s", ev.EventType, err, raw)
		}
		var generic map[string]any
		_ = json.Unmarshal(raw, &generic)
		for _, forbidden := range []string{"tokens", "cost", "turn_index", "gateway_request_id", "otel_request_id", "proxy_request_id"} {
			if _, present := generic[forbidden]; present {
				t.Errorf("%s carries %s", ev.EventType, forbidden)
			}
		}
		// Only the started half carries a span, and in it only the request.
		span, hasSpan := generic["span"].(map[string]any)
		if wantSpan := ev.EventType == client.EventModelCallRequested; hasSpan != wantSpan {
			t.Errorf("%s: span present = %v, want %v", ev.EventType, hasSpan, wantSpan)
		}
		if hasSpan && (span["request_body"] == nil || span["response_body"] != nil || span["http_url"] != nil) {
			t.Errorf("%s: span = %v, want request_body only", ev.EventType, span)
		}
		if generic["model_call_request_id"] != "turn-0001:0:1.1" {
			t.Errorf("%s: model_call_request_id = %v", ev.EventType, generic["model_call_request_id"])
		}
	}
}
