package client

import (
	"encoding/json"
	"strings"
	"testing"
)

// The assistant-turn span. Every assertion below is against a consumer that
// fails SILENTLY: core logs and returns "" for a span it cannot read, so a wrong
// shape here does not error anywhere — it just leaves the Goal Alignment widgets
// empty exactly as they are today.

func turnEvent(text string) DevEvent {
	idx := 3
	ev := DevEvent{
		SchemaVersion: SchemaVersion,
		EventID:       "ev-turn",
		EventType:     EventTurnCompleted,
		SessionID:     "sess-turn",
		DeveloperDID:  "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		Timestamp:     "2026-08-13T10:00:12Z",
		StartedAt:     "2026-08-13T10:00:00Z",
		EndedAt:       "2026-08-13T10:00:12Z",
		Tool:          Tool{Name: "claude-code", Kind: ToolShell},
		TurnIndex:     &idx,
		Model:         "claude-opus-4-8",
	}
	if text != "" {
		ev.Content = &Content{Output: text}
	}
	return ev
}

// decodeSpans pulls the spans array out of the wire payload as core receives it.
func decodeSpans(t *testing.T, ev DevEvent) []map[string]any {
	t.Helper()
	m := decodeRaw(t, ev)
	raw, present := m["spans"]
	if !present {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("spans is not an array: %T", raw)
	}
	out := make([]map[string]any, 0, len(list))
	for _, s := range list {
		sm, ok := s.(map[string]any)
		if !ok {
			t.Fatalf("span is not an object: %T", s)
		}
		out = append(out, sm)
	}
	return out
}

// The four conditions core's extractor checks, asserted together, because
// failing any one of them yields the same symptom: an empty widget.
func TestTurnSpanSatisfiesTheAlignmentExtractor(t *testing.T) {
	const text = "I refactored the spool and all 11 modules are green."
	spans := decodeSpans(t, turnEvent(text))
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want exactly 1", len(spans))
	}
	s := spans[0]

	if s["stage"] != "completed" {
		t.Errorf("stage = %v, want \"completed\" (extractor rejects anything else)", s["stage"])
	}
	if s["semantic_type"] != "llm_completion" {
		t.Errorf("semantic_type = %v, want llm_completion", s["semantic_type"])
	}
	// isLLMCall's inputs. Without both, core classifies the span as something
	// else and the extractor's semantic_type check fails.
	attrs, _ := s["attributes"].(map[string]any)
	if attrs["http.method"] != "POST" {
		t.Errorf("attributes[http.method] = %v, want POST", attrs["http.method"])
	}
	url, _ := attrs["http.url"].(string)
	if !strings.Contains(url, "api.anthropic.com") {
		t.Errorf("attributes[http.url] = %q, must contain an LLM domain core knows", url)
	}
	if attrs["openbox.span_synthetic"] != true {
		t.Error("the synthetic marker is missing — stored spans would be " +
			"indistinguishable from observed HTTP traffic")
	}
	// The name must not classify as embedding or tool-call.
	name, _ := s["name"].(string)
	upper := strings.ToUpper(name)
	if strings.Contains(upper, "EMBED") || strings.Contains(upper, "TOOL") {
		t.Errorf("span name %q contains EMBED/TOOL and would classify as something "+
			"other than llm_completion (session.go:323-334)", name)
	}

	// And the body parses back to exactly the text, through the shape core
	// unmarshals.
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	body, _ := s["response_body"].(string)
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("response_body is not the shape core unmarshals: %v (%q)", err, body)
	}
	if len(parsed.Choices) == 0 || parsed.Choices[0].Message.Content != text {
		t.Errorf("round-tripped content = %+v, want %q", parsed.Choices, text)
	}
}

// The gate, from the client side. Emit's stripContent nils Content when capture
// is off; this asserts the consequence — not an empty array, not a zero count,
// nothing at all.
func TestTurnSpanAbsentWithoutContent(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   DevEvent
	}{
		{"no content at all", turnEvent("")},
		{"content stripped by the gate", stripContent(turnEvent("secret assistant words"))},
		{"empty output field", func() DevEvent { e := turnEvent(""); e.Content = &Content{}; return e }()},
	} {
		m := decodeRaw(t, tc.ev)
		for _, k := range []string{"spans", "span_count"} {
			if v, present := m[k]; present {
				t.Errorf("%s: payload carries %q = %v; nothing new may egress with capture off", tc.name, k, v)
			}
		}
	}
}

// span_count must agree with the array, or core's own bookkeeping disagrees
// with what it stored.
func TestTurnSpanCountIsOne(t *testing.T) {
	m := decodeRaw(t, turnEvent("hello"))
	if m["span_count"] != float64(1) {
		t.Errorf("span_count = %v, want 1", m["span_count"])
	}
}

// hook_trigger must never appear. With it true AND spans present the payload
// enters core's approval-bypass fingerprint path — a model turn is not an
// approvable operation and must not touch it.
func TestTurnSpanCarriesNoHookTrigger(t *testing.T) {
	m := decodeRaw(t, turnEvent("hello"))
	if v, present := m["hook_trigger"]; present {
		t.Errorf("hook_trigger = %v; with spans present this enters the approval-bypass "+
			"fingerprint path (governance_workflow.go:310-330)", v)
	}
}

// Ids must be a pure function of the event. The turn cursor re-reads a window
// after a crash on purpose — over-report into a server that deduplicates rather
// than lose a turn — and core dedupes spans on (span_id, stage). Random ids
// would turn that safe direction into a second stored copy of the assistant's
// text.
func TestTurnSpanIDsAreDeterministic(t *testing.T) {
	a := decodeSpans(t, turnEvent("same turn, reported twice"))[0]
	b := decodeSpans(t, turnEvent("same turn, reported twice"))[0]
	if a["span_id"] != b["span_id"] || a["trace_id"] != b["trace_id"] {
		t.Errorf("ids are not stable across two derivations: %v/%v vs %v/%v",
			a["span_id"], a["trace_id"], b["span_id"], b["trace_id"])
	}
	// Core's OTel id widths.
	if id, _ := a["span_id"].(string); len(id) != 16 {
		t.Errorf("span_id = %q (%d chars), want 16 hex", id, len(id))
	}
	if id, _ := a["trace_id"].(string); len(id) != 32 {
		t.Errorf("trace_id = %q (%d chars), want 32 hex", id, len(id))
	}

	// A DIFFERENT turn must get a different span id, or two turns collapse onto
	// one row under the same dedupe.
	other := turnEvent("a different turn")
	idx := 4
	other.TurnIndex = &idx
	if c := decodeSpans(t, other)[0]; c["span_id"] == a["span_id"] {
		t.Error("two different turns derived the same span_id; core would dedupe one away")
	}
	// Turns of one session share a trace.
	if c := decodeSpans(t, other)[0]; c["trace_id"] != a["trace_id"] {
		t.Error("turns of the same session must share a trace_id")
	}
}

// The cap bounds the TEXT, not the envelope — and it is applied before the JSON
// wrapper, so the body still parses. A cap applied after wrapping could cut
// mid-escape and produce a response_body core reports as silence.
func TestTurnSpanCapsTheTextNotTheEnvelope(t *testing.T) {
	long := strings.Repeat("x", maxBodySize+5000)
	s := decodeSpans(t, turnEvent(long))[0]
	body, _ := s["response_body"].(string)

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("an over-cap body must still parse; got %v", err)
	}
	if got := len([]rune(parsed.Choices[0].Message.Content)); got != maxBodySize {
		t.Errorf("capped content = %d runes, want %d", got, maxBodySize)
	}
}

// No other event type may grow a span. The carve-out is exactly one carrier.
func TestNoOtherEventCarriesASpan(t *testing.T) {
	for _, et := range []EventType{
		EventSessionStarted, EventSessionEnded, EventPromptSubmitted, EventDeploy,
		EventToolCall, EventToolResult, EventTurnStarted,
		EventSubagentStarted, EventPermissionDenied, EventAPIError,
	} {
		ev := turnEvent("assistant text that must not ride this event")
		ev.EventType = et
		if et == EventToolCall || et == EventToolResult {
			ev.Span = &Span{SemanticType: "internal", Stage: "completed"}
		}
		m := decodeRaw(t, ev)
		if v, present := m["spans"]; present {
			t.Errorf("%s carries spans = %v; only TurnCompleted may", et, v)
		}
	}
}
