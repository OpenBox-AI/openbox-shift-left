package muse

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUsageNumbersIsAnAllowlistOfIntegers(t *testing.T) {
	got := usageNumbers(json.RawMessage(`{
		"input_tokens":1200,"output_tokens":"340","cached_tokens":800,"cache_read_tokens":800,"reasoning_tokens":96,
		"prompt":"the user's text","cost_usd":0.12,"input_tokens_details":{"a":1},"total_tokens":-4}`))
	want := map[string]int{"input_tokens": 1200, "output_tokens": 340, "cached_tokens": 800, "cache_read_tokens": 800, "reasoning_tokens": 96}
	if len(got) != len(want) {
		t.Fatalf("usage = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d", k, got[k], v)
		}
	}
	for _, bad := range []string{``, `null`, `"text"`, `[1]`, `{}`, `{"prompt":"x"}`} {
		if u := usageNumbers(json.RawMessage(bad)); u != nil {
			t.Errorf("usageNumbers(%s) = %v", bad, u)
		}
	}
}

func TestTraceparentMustBeWellFormed(t *testing.T) {
	const tp = "00-00000000000000000000000000000001-0000000000000001-01"
	// Muse 1.4.1 sends options as a flat map with dotted keys; the nested form
	// is only a fallback.
	for name, ok := range map[string]string{
		"flat":   `{"meta.traceparent":"` + tp + `","meta.reasoning.effort":"low"}`,
		"nested": `{"meta":{"traceparent":"` + tp + `","reasoning":{"effort":"medium"}}}`,
	} {
		if got := traceparentOf(json.RawMessage(ok)); got != tp {
			t.Errorf("%s: traceparent = %q", name, got)
		}
	}
	for _, bad := range []string{`{"meta.traceparent":"not-a-trace-id"}`, `{"meta.traceparent":7}`, `{"meta":{"traceparent":"not-a-trace-id"}}`, `{"meta":{"traceparent":"00-ZZ-00-01"}}`, `{"meta":{}}`, `{}`, `[]`, ``} {
		if got := traceparentOf(json.RawMessage(bad)); got != "" {
			t.Errorf("traceparentOf(%s) = %q", bad, got)
		}
	}
}

// Nothing in this package may put usage on a DevEvent: the gate row is not a
// completion, and the client would drop it, but the adapter never sets it.
func TestNoFixtureEverProducesUsage(t *testing.T) {
	m := testMapper()
	m.CaptureContent = true
	for name := range fixtureEvents {
		ev := parseFixture(t, name)
		got, ok := m.Map(hookOf(ev), ev)
		if !ok {
			continue
		}
		raw, _ := json.Marshal(got)
		s := string(raw)
		for _, forbidden := range []string{`"tokens"`, `"cost"`, `"turn_index"`, `input_tokens`, `cache_read_tokens`, `reasoning_tokens`, `"usage"`, `traceparent`} {
			if strings.Contains(s, forbidden) {
				t.Errorf("%s: event carries %s: %s", name, forbidden, s)
			}
		}
		if got.ActivityType == "llm_completion" || got.EventType == "TurnStarted" || got.EventType == "TurnCompleted" {
			t.Errorf("%s: produced a model-call record (%s)", name, got.EventType)
		}
	}
}
