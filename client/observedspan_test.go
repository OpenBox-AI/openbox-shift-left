package client

import (
	"encoding/json"
	"strings"
	"testing"
)

// The observed-span path was gateway-only, and gating it on GatewayRequestID
// alone became a latent defect the moment ADR-0022 declared OtelRequestID and
// ProxyRequestID. An event carrying one of those plus a populated Span was
// accepted, spooled, signed and POSTed with NO span attached — the signature
// failure shape in this repo: a lane that looks like it works and carries none
// of its evidence.
//
// These tests hold the fix in all four directions that can regress: the new
// lanes get a span, the gateway's own ids did not move, the three lanes never
// collide, and a turn with no lane still takes the hook path.

// spanOf returns the single span the payload carries, or false when it carries
// none. It goes through buildPayload rather than calling the builder, because
// the defect was in the CALL SITE's gate, not in the builder.
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

// turnWithLane builds a TurnCompleted carrying observed HTTP evidence, with the
// lane discriminator the caller names.
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
			// The classification keys are what make core recompute
			// semantic_type as llm_completion; without them the span stores,
			// classifies as something else, and every reader goes quiet.
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
//
// Generalizing the builder had to leave the gateway's ids byte-identical: they
// are already stored core-side, and core's span dedupe is (span_id, stage)
// scoped by session — so a changed derivation would stop absorbing a re-emit and
// start writing a second row for the same call.
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

// TestObservingLaneSpanIDsAreDisjoint is the control that matters most.
//
// Two lanes can describe the SAME turn. If they minted the same span id, core's
// dedupe would absorb one as a duplicate of the other and half the evidence
// would vanish with no error anywhere — the same failure the activity_id
// namespaces exist to prevent, one level down.
//
// Two mechanisms carry it — the lane in the hash input and the lane in the
// prefix — and they are REDUNDANT: drilled 2026-08-28, removing either alone
// leaves this test green, and only removing both turns it red. So this test
// pins the property, not either mechanism; do not read a green run as evidence
// that both are still present.
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
// drifting apart.
//
// If observedLane and turnActivityIDFor disagreed, one event could take its
// activity_id from one lane and its span id from another, and core would file
// half the evidence under a row the other half never joins.
func TestLanePrecedenceMatchesActivityID(t *testing.T) {
	// A deliberately malformed event carrying all three: the contract's
	// turnProducer oneOf rejects this shape, so the only thing under test is
	// that both functions resolve it the SAME way.
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
