package client

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests hold the fix in all four directions that can regress: the new
// lanes get a span, the gateway's own ids did not move, the three lanes never
// collide, and a turn with no lane still takes the hook path.

// spanOf returns the single span the payload carries, or false when it carries
// none. It goes through buildPayload rather than calling the builder, because
// the defect was in the call site's gate, not in the builder.
func spanOf(t *testing.T, ev DevEvent) (map[string]any, bool) {
	t.Helper()
	b, err := buildPayload(ev)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	var p struct {
		Spans     []map[string]any `json:"spans"`
		SpanCount int              `json:"span_count"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(p.Spans) == 0 {
		return nil, false
	}
	if p.SpanCount != len(p.Spans) {
		t.Errorf("span_count %d disagrees with %d spans", p.SpanCount, len(p.Spans))
	}
	return p.Spans[0], true
}

func turnWithLane(lane, id string) DevEvent {
	ev := DevEvent{
		SchemaVersion: SchemaVersion,
		EventType:     EventTurnCompleted,
		SessionID:     "s1",
		DeveloperDID:  "did:openbox:dev:test",
		Timestamp:     "2026-08-28T10:00:00Z",
		Tool:          Tool{Name: "claude-code", Kind: ToolShell},
		Span: &Span{
			HTTPMethod: "POST",
			HTTPURL:    "https://api.anthropic.com/v1/messages",
			HTTPStatus: 200,
		},
	}
	switch lane {
	case "gw":
		ev.GatewayRequestID = id
	case "otel":
		ev.OtelRequestID = id
	case "proxy":
		ev.ProxyRequestID = id
	}
	return ev
}

func TestObservedSpanShipsForEveryObservingLane(t *testing.T) {
	for _, lane := range []string{"gw", "otel", "proxy"} {
		t.Run(lane, func(t *testing.T) {
			span, ok := spanOf(t, turnWithLane(lane, lane+"-req-1"))
			if !ok {
				t.Fatalf("%s turn carries NO span — the lane's evidence is being dropped silently", lane)
			}
			if got := span["http_method"]; got != "POST" {
				t.Errorf("http_method = %v, want POST", got)
			}
			if got, _ := span["http_status_code"].(float64); got != 200 {
				t.Errorf("http_status_code = %v, want 200 (core's SpanData spells it _code; the short spelling is dropped silently)", span["http_status_code"])
			}
			if !strings.HasPrefix(span["span_id"].(string), lane+"-") {
				t.Errorf("span_id %q is not in the %s namespace", span["span_id"], lane)
			}
		})
	}
}

// TestGatewaySpanIDDidNotMove pins the gateway derivation against a value
// computed independently of this code.
func TestGatewaySpanIDDidNotMove(t *testing.T) {
	span, ok := spanOf(t, turnWithLane("gw", "gw-req-1"))
	if !ok {
		t.Fatal("gateway turn carries no span")
	}
	const want = "gw-ed6bb44e646a179c7357a51108c3fc32"
	if got := span["span_id"]; got != want {
		t.Errorf("gateway span_id = %v, want %s — a moved derivation orphans every stored id", got, want)
	}
}

// TestObservingLaneSpanIDsAreDisjoint is the control that matters most. So
// this test pins the property, not either mechanism; do not read a green run
// as evidence that both are still present.
func TestObservingLaneSpanIDsAreDisjoint(t *testing.T) {
	const shared = "same-id"
	seen := map[string]string{}
	for _, lane := range []string{"gw", "otel", "proxy"} {
		ev := turnWithLane(lane, shared)
		id := observedSpanID(ev)
		if id == "" {
			t.Fatalf("%s: no span id", lane)
		}
		if prev, dup := seen[id]; dup {
			t.Fatalf("%s and %s mint the SAME span id %q for the same turn — core's dedupe would drop one lane's evidence", lane, prev, id)
		}
		seen[id] = lane
	}
}

// TestLanePrecedenceMatchesActivityID keeps the two namespace decisions from
// drifting apart. If observedLane and turnActivityIDFor disagreed, one event
// could take its activity_id from one lane and its span id from another, and
// core would file half the evidence under a row the other half never joins.
func TestLanePrecedenceMatchesActivityID(t *testing.T) {
	ev := DevEvent{SessionID: "s1"}
	ev.ProxyRequestID, ev.GatewayRequestID, ev.OtelRequestID = "p", "g", "o"

	lane, id := observedLane(ev)
	if lane != "proxy" || id != "p" {
		t.Fatalf("observedLane = (%q,%q), want (proxy,p)", lane, id)
	}
	if got := turnActivityIDFor(ev); got != "s1:proxy:p" {
		t.Fatalf("turnActivityIDFor = %q, want s1:proxy:p — the two precedences have drifted", got)
	}

	ev.ProxyRequestID = ""
	if lane, _ := observedLane(ev); lane != "gw" {
		t.Fatalf("without proxy, observedLane = %q, want gw", lane)
	}
	if got := turnActivityIDFor(ev); got != "s1:gateway:g" {
		t.Fatalf("without proxy, turnActivityIDFor = %q, want s1:gateway:g", got)
	}

	ev.GatewayRequestID = ""
	if lane, _ := observedLane(ev); lane != "otel" {
		t.Fatalf("with only otel, observedLane = %q, want otel", lane)
	}
	if got := turnActivityIDFor(ev); got != "s1:otel:o" {
		t.Fatalf("with only otel, turnActivityIDFor = %q, want s1:otel:o", got)
	}
}

// TestNoLaneStillTakesTheHookPath is the regression guard on the other side: a
// hook-only install must emit exactly what it emitted before. A turn with no
// lane discriminator carries the assistant-text span or nothing, never an
// observed one.
func TestNoLaneStillTakesTheHookPath(t *testing.T) {
	ev := turnWithLane("", "")
	if id := observedSpanID(ev); id != "" {
		t.Errorf("a turn with no observing lane minted span id %q", id)
	}
	if span := observedSpan(ev); span != nil {
		t.Errorf("a turn with no observing lane built an observed span: %+v", span)
	}
}

// TestOnlyTheTelemetryLaneMarksItsSpanSynthetic pins the honesty control. The
// marker is derived from the lane, not set by the caller, so a mapper cannot
// forget it.
func TestOnlyTheTelemetryLaneMarksItsSpanSynthetic(t *testing.T) {
	for lane, wantMarked := range map[string]bool{
		"otel":  true,  // client-asserted: the http.* keys are synthesized
		"gw":    false, // in-path: it saw them
		"proxy": false, // in-path: it saw them
	} {
		t.Run(lane, func(t *testing.T) {
			span, ok := spanOf(t, turnWithLane(lane, lane+"-req-1"))
			if !ok {
				t.Fatalf("%s: no span", lane)
			}
			attrs, _ := span["attributes"].(map[string]any)
			marked, _ := attrs["openbox.span_synthetic"].(bool)
			if marked != wantMarked {
				t.Errorf("%s: openbox.span_synthetic = %v, want %v", lane, marked, wantMarked)
			}
			if attrs["http.method"] != "POST" {
				t.Errorf("%s: http.method = %v, want POST", lane, attrs["http.method"])
			}
			if attrs["http.url"] == nil {
				t.Errorf("%s: no http.url — isLLMCall needs it", lane)
			}
		})
	}
}
