package client

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/conformance"
)

// gateEvent builds one half of a model-call gate pair with every field a
// careless adapter could set that the gate must not put on the wire.
func gateEvent(et EventType) DevEvent {
	in, out := 100, 50
	idx := 3
	return DevEvent{
		SchemaVersion:      SchemaVersion,
		EventID:            "ev-gate-" + string(et),
		EventType:          et,
		SessionID:          "sess-gate",
		DeveloperDID:       "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		Timestamp:          "2026-09-30T10:00:12Z",
		StartedAt:          "2026-09-30T10:00:00Z",
		EndedAt:            "2026-09-30T10:00:12Z",
		Tool:               Tool{Name: "muse", Kind: ToolShell},
		Model:              "muse-spark",
		ModelCallRequestID: "req_01HZ.1",
		Status:             StatusCompleted,
		ActivityType:       ActivityTypeLLMCompletion, // must be overridden, never forwarded
		Tokens:             &Tokens{Input: &in, Output: &out},
		TurnIndex:          &idx,
		ProxyRequestID:     "px-1",
		GatewayRequestID:   "gw-1",
		OtelRequestID:      "ot-1",
		Span:               &Span{SemanticType: "llm", Stage: "started", RequestBody: gateRequestBody, HTTPMethod: "POST", HTTPURL: "http://x/y", ResponseBody: "resp"},
		Metadata: map[string]any{
			"provider":         "meta",
			"message_count":    4,
			"tool_count":       2,
			"tool_names":       []string{"Bash", "Read"},
			"message_previews": []string{"hello there", "second message"},
			"finish_reason":    "stop",
			"response_id":      "resp-9",
			"error_class":      "rate_limit",
			"tool_call_count":  1,
		},
	}
}

const gateRequestBody = `{"messages":[{"role":"user","content":[{"type":"text","text":"hello there"}]}],"tools":[]}`

func gateWire(t *testing.T, ev DevEvent) map[string]any {
	t.Helper()
	return decodeRaw(t, ev)
}

func objectField(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	o, _ := m[key].(map[string]any)
	if o == nil {
		t.Fatalf("wire %q is absent or not an object: %v", key, m[key])
	}
	return o
}

func TestModelCallGateVocabulary(t *testing.T) {
	if SchemaVersion != "1.11" {
		t.Errorf("SchemaVersion = %q, want 1.11", SchemaVersion)
	}
	if EventModelCallRequested != "ModelCallRequested" || EventModelCallFinished != "ModelCallFinished" {
		t.Errorf("event names drifted: %q %q", EventModelCallRequested, EventModelCallFinished)
	}
	if ActivityTypeModelCallGate != "model_call_gate" {
		t.Errorf("ActivityTypeModelCallGate = %q", ActivityTypeModelCallGate)
	}
	for _, want := range []string{"model_call_gate"} {
		found := false
		for _, a := range AllActivityTypes {
			found = found || a == want
		}
		if !found {
			t.Errorf("AllActivityTypes lacks %q", want)
		}
	}
	for et, want := range map[EventType]string{
		EventModelCallRequested: wireActivityStarted,
		EventModelCallFinished:  wireActivityCompleted,
	} {
		got, sig, err := wireTypeFor(et)
		if err != nil || got != want || sig != "" {
			t.Errorf("wireTypeFor(%s) = %q %q %v, want %q", et, got, sig, err, want)
		}
	}
}

// TestModelCallGateWireShape asserts on the serialized bytes, never the struct:
// a struct field that is set but dropped by omitempty and a key that never
// reaches the wire are indistinguishable there.
func TestModelCallGateWireShape(t *testing.T) {
	for _, et := range []EventType{EventModelCallRequested, EventModelCallFinished} {
		t.Run(string(et), func(t *testing.T) {
			b, err := buildPayload(gateEvent(et))
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			if m["activity_type"] != "model_call_gate" {
				t.Errorf("activity_type = %v, want model_call_gate", m["activity_type"])
			}
			if m["activity_id"] != "sess-gate:llmgate:req_01HZ.1" {
				t.Errorf("activity_id = %v", m["activity_id"])
			}
			if _, present := m["spans"]; present {
				t.Error("a gate row carries spans[]")
			}
			if _, present := m["hook_trigger"]; present {
				t.Error("a gate row carries hook_trigger")
			}
			// Usage is dropped wherever it could ride: activity_output, metadata,
			// and a top-level key.
			for _, banned := range []string{`"usage"`, `"tokens"`, `input_tokens`, `output_tokens`,
				`"turn_index"`, `"proxy_request_id"`, `"otel_request_id"`, `"gateway_request_id"`,
				`"reply_text"`, `llm_completion`} {
				if strings.Contains(string(b), banned) {
					t.Errorf("gate wire carries %s: %s", banned, b)
				}
			}
		})
	}
}

func TestModelCallGateStartedInput(t *testing.T) {
	m := gateWire(t, gateEvent(EventModelCallRequested))
	in := objectField(t, m, "activity_input")
	if in["model"] != "muse-spark" || in["provider"] != "meta" {
		t.Errorf("model/provider missing from activity_input: %v", in)
	}
	if in["message_count"] != float64(4) || in["tool_count"] != float64(2) {
		t.Errorf("counts missing from activity_input: %v", in)
	}
	names, _ := in["tool_names"].([]any)
	if len(names) != 2 || names[0] != "Bash" {
		t.Errorf("tool_names = %v", in["tool_names"])
	}
	if in["content"] != gateRequestBody {
		t.Errorf("content = %v, want the request body", in["content"])
	}
	if _, present := in["message_previews"]; present {
		t.Error("message_previews is retired and must not reach activity_input")
	}
	for _, k := range []string{"http_method", "http_url", "request_body", "response_body"} {
		if _, present := in[k]; present {
			t.Errorf("activity_input carries span field %s", k)
		}
	}
	if _, present := m["activity_output"]; present {
		t.Error("the started half carries activity_output")
	}
	if _, present := m["status"]; present {
		t.Error("the started half carries status")
	}
	for _, completionOnly := range []string{"finish_reason", "response_id", "error_class", "tool_call_count"} {
		if _, present := in[completionOnly]; present {
			t.Errorf("activity_input carries completion-only key %s", completionOnly)
		}
	}
}

func objectRaw(t *testing.T, m map[string]any, key string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(objectField(t, m, key))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestModelCallGateContentFollowsCapture is the gate proper: with capture off
// the request reaches neither activity_input, metadata nor spans, while the
// structural keys still ship.
func TestModelCallGateContentFollowsCapture(t *testing.T) {
	const canary = "REQUEST-CANARY"
	ev := gateEvent(EventModelCallRequested)
	ev.Span.RequestBody = canary

	on, _ := buildPayload(ev)
	if !strings.Contains(string(on), canary) {
		t.Fatal("capture on: request body missing from the wire")
	}

	off, err := buildPayload(stripContent(ev))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(off), canary) {
		t.Errorf("capture off: request body reached the wire: %s", off)
	}
	in := objectField(t, decodeRawBytes(t, off), "activity_input")
	if _, present := in["content"]; present {
		t.Errorf("capture off: content key present: %v", in)
	}
	if in["message_count"] != float64(4) || in["provider"] != "meta" || in["model"] != "muse-spark" {
		t.Errorf("capture off: structural keys lost: %v", in)
	}
}

// TestModelCallGateContentIsTailCapped: an over-cap request keeps its newest
// end, in bytes, and the cut is indexed in truncated_paths.
func TestModelCallGateContentIsTailCapped(t *testing.T) {
	ev := gateEvent(EventModelCallRequested)
	body := strings.Repeat("A", maxModelCallBodyBytes) + "NEWEST-TURN"
	ev.Span.RequestBody = body
	m := gateWire(t, ev)
	in := objectField(t, m, "activity_input")
	got, _ := in["content"].(string)
	if want := capModelCallRequest(body); got != want || len(got) != maxModelCallBodyBytes {
		t.Fatalf("content len %d, want %d (the tail cap)", len(got), len(capModelCallRequest(body)))
	}
	if !strings.HasSuffix(got, "NEWEST-TURN") || !strings.HasPrefix(got, truncationMark) {
		t.Error("tail cap did not keep the newest end")
	}
	if !strings.Contains(string(objectRaw(t, m, "metadata")), "activity_input.content") {
		t.Errorf("cut not indexed in truncated_paths: %v", m["metadata"])
	}

	ev.Span.RequestBody = "short"
	if md := string(objectRaw(t, gateWire(t, ev), "metadata")); strings.Contains(md, "activity_input.content") {
		t.Errorf("an uncut body was indexed as cut: %s", md)
	}
}

// TestModelCallGateFinishedCarriesNoContent: only the started half keeps the
// request; a finished half drops every span field.
func TestModelCallGateFinishedCarriesNoContent(t *testing.T) {
	ev := gateEvent(EventModelCallFinished)
	b, err := buildPayload(ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "hello there") || strings.Contains(string(b), `"content"`) {
		t.Errorf("finished half carries request content: %s", b)
	}
	if got := normalizeModelCallGate(ev); got.Span != nil {
		t.Errorf("finished half kept a span: %+v", got.Span)
	}
	if got := normalizeModelCallGate(gateEvent(EventModelCallRequested)); got.Span == nil || !reflect.DeepEqual(*got.Span, Span{RequestBody: gateRequestBody}) {
		t.Errorf("started half span = %+v, want only RequestBody", got.Span)
	}
}

func decodeRawBytes(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestModelCallPreviewKeyIsAContentKey(t *testing.T) {
	if !contentMetadataKeys["message_previews"] {
		t.Error("message_previews is not in contentMetadataKeys; content would route around the gate")
	}
}

func TestModelCallGateFinishedOutputIsMetadataOnly(t *testing.T) {
	ev := gateEvent(EventModelCallFinished)
	ev.Content = &Content{Output: "MODEL-REPLY", Thinking: "MODEL-THOUGHT"}
	ev.Span = &Span{ResponseBody: "MODEL-BODY"}
	b, err := buildPayload(ev)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"MODEL-REPLY", "MODEL-THOUGHT", "MODEL-BODY"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("gate completion carries content %q: %s", leak, b)
		}
	}
	m := decodeRawBytes(t, b)
	out := objectField(t, m, "activity_output")
	want := map[string]any{
		"status": "completed", "finish_reason": "stop", "response_id": "resp-9",
		"error_class": "rate_limit", "tool_call_count": float64(1),
		"model": "muse-spark", "provider": "meta",
	}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("activity_output[%s] = %v, want %v", k, out[k], v)
		}
	}
	if len(out) != len(want) {
		t.Errorf("activity_output carries extra keys: %v", out)
	}
	if _, present := m["activity_input"]; present {
		t.Error("the completed half carries activity_input")
	}
	if m["status"] != "completed" {
		t.Errorf("wire status = %v, want completed", m["status"])
	}
}

func TestModelCallFinishedStatusFollowsStatusFor(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{StatusCompleted, "completed"}, {StatusFailed, "failed"}, {"success", ""}, {"", ""},
	} {
		ev := gateEvent(EventModelCallFinished)
		ev.Status = tc.in
		m := gateWire(t, ev)
		got, _ := m["status"].(string)
		if got != tc.want {
			t.Errorf("status %q: wire status = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Still scoped: the started half never carries one.
	if _, present := gateWire(t, gateEvent(EventModelCallRequested))["status"]; present {
		t.Error("ModelCallRequested carries status")
	}
}

func TestModelCallGatePairsOnOneActivityID(t *testing.T) {
	a := decodePayload(t, gateEvent(EventModelCallRequested))
	b := decodePayload(t, gateEvent(EventModelCallFinished))
	if a.ActivityID == "" || a.ActivityID != b.ActivityID {
		t.Errorf("halves do not share an activity_id: %q vs %q", a.ActivityID, b.ActivityID)
	}
	if a.EventType != wireActivityStarted || b.EventType != wireActivityCompleted {
		t.Errorf("wire types = %s/%s", a.EventType, b.EventType)
	}
	if WireActivityID(gateEvent(EventModelCallRequested)) != a.ActivityID ||
		WireActivityID(gateEvent(EventModelCallFinished)) != a.ActivityID {
		t.Error("WireActivityID disagrees with the payload")
	}
}

// TestModelCallGateNamespaceIsDisjoint: one id text, every producer, no two
// activity ids equal.
func TestModelCallGateNamespaceIsDisjoint(t *testing.T) {
	const id = "same-id.1"
	idx := 1
	ids := map[string]string{}
	add := func(name string, ev DevEvent) {
		ev.SessionID = "s"
		got := WireActivityID(ev)
		if got == "" {
			t.Fatalf("%s: no activity id", name)
		}
		if prev, dup := ids[got]; dup {
			t.Errorf("%s and %s share activity id %q", name, prev, got)
		}
		ids[got] = name
	}
	base := func(et EventType) DevEvent { return DevEvent{EventType: et, SessionID: "s"} }
	add("llmgate", func() DevEvent { e := base(EventModelCallRequested); e.ModelCallRequestID = id; return e }())
	add("proxy", func() DevEvent { e := base(EventTurnStarted); e.ProxyRequestID = id; return e }())
	add("otel", func() DevEvent { e := base(EventTurnStarted); e.OtelRequestID = id; return e }())
	add("gateway", func() DevEvent { e := base(EventTurnStarted); e.GatewayRequestID = id; return e }())
	add("turn", func() DevEvent { e := base(EventTurnStarted); e.TurnIndex = &idx; return e }())
	add("rollup", func() DevEvent { e := base(EventTurnStarted); e.SessionRollup = true; return e }())
	add("tool", func() DevEvent { e := base(EventToolCall); e.Tool = Tool{Name: id}; return e }())
}

func TestModelCallRequestIDBound(t *testing.T) {
	good := []string{"a", "req_01HZ.1", strings.Repeat("x", 128)}
	bad := []string{"", strings.Repeat("x", 129), "a b", "a\nb", "a\x01b", "réq", "a\x7fb"}
	for _, id := range good {
		if !UsableModelCallRequestID(id) {
			t.Errorf("%q rejected", id)
		}
		ev := gateEvent(EventModelCallRequested)
		ev.ModelCallRequestID = id
		if _, err := buildPayload(ev); err != nil {
			t.Errorf("%q: buildPayload: %v", id, err)
		}
	}
	for _, id := range bad {
		if UsableModelCallRequestID(id) {
			t.Errorf("%q accepted", id)
		}
		for _, et := range []EventType{EventModelCallRequested, EventModelCallFinished} {
			ev := gateEvent(et)
			ev.ModelCallRequestID = id
			if _, err := buildPayload(ev); err == nil {
				t.Errorf("%s with id %q built; an unbounded id would reach a stored key", et, id)
			}
		}
	}
}

// TestModelCallRequestIDOnlyRidesTheGate: the field is meaningless on another
// type and must not move that type's activity id.
func TestModelCallRequestIDOnlyRidesTheGate(t *testing.T) {
	idx := 0
	ev := DevEvent{EventType: EventTurnStarted, SessionID: "s", TurnIndex: &idx, ModelCallRequestID: "x"}
	if got := WireActivityID(ev); got != "s:turn:0" {
		t.Errorf("turn activity id = %q", got)
	}
}

// TestModelCallGateValidatesAgainstTheSchema runs the serialized adapter-facing
// event, not the wire, through the contract.
func TestModelCallGateValidatesAgainstTheSchema(t *testing.T) {
	for _, et := range []EventType{EventModelCallRequested, EventModelCallFinished} {
		ev := gateEvent(et)
		// The adapter-facing contract forbids the extra discriminators the wire
		// test above plants to prove they are dropped.
		ev.ActivityType = ActivityTypeModelCallGate
		ev.Tokens, ev.TurnIndex, ev.Span = nil, nil, nil
		ev.ProxyRequestID, ev.GatewayRequestID, ev.OtelRequestID = "", "", ""
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if err := conformance.ValidateDevEvent(raw, true); err != nil {
			t.Errorf("%s: %v", et, err)
		}
	}
}

// TestApprovalKeyForAModelCallGateIsItsWireActivity an approval hold polls by
// the key core filed the record under; for a gate that is its :llmgate: id.
func TestApprovalKeyForAModelCallGateIsItsWireActivity(t *testing.T) {
	ev := DevEvent{EventType: EventModelCallRequested, SessionID: "s1", ModelCallRequestID: "req-1.1"}
	if got, want := ApprovalKeyFor(ev).ActivityID, WireActivityID(ev); got != want {
		t.Fatalf("ApprovalKeyFor activity = %q, want the wire activity %q", got, want)
	}
}
