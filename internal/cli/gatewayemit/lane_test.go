package gatewayemit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
)

// Lane_test.go; the two in-path model-call producers share this emitter. The
// namespaces are client's (turnActivityIDFor); what this file pins is the half
// that lives here: which discriminator field a lane writes, and that a lane
// nobody configured cannot silently borrow another lane's.

// TestEachLaneWritesOnlyItsOwnDiscriminator.
func TestEachLaneWritesOnlyItsOwnDiscriminator(t *testing.T) {
	t.Run("gateway", func(t *testing.T) {
		ev, err := onlyCompleted(EventsFor(LaneGateway, sampleIdentity(), "req-1", sampleAt, sampleCaptured()))
		if err != nil {
			t.Fatalf("EventFor: %v", err)
		}
		if ev.GatewayRequestID != "req-1" {
			t.Errorf("GatewayRequestID = %q, want req-1", ev.GatewayRequestID)
		}
		if ev.ProxyRequestID != "" || ev.OtelRequestID != "" {
			t.Errorf("a gateway event carries another lane's discriminator: proxy=%q otel=%q",
				ev.ProxyRequestID, ev.OtelRequestID)
		}
	})

	t.Run("proxy", func(t *testing.T) {
		ev, err := onlyCompleted(EventsFor(LaneProxy, sampleIdentity(), "req-1", sampleAt, sampleCaptured()))
		if err != nil {
			t.Fatalf("EventFor: %v", err)
		}
		if ev.ProxyRequestID != "req-1" {
			t.Errorf("ProxyRequestID = %q, want req-1", ev.ProxyRequestID)
		}
		if ev.GatewayRequestID != "" || ev.OtelRequestID != "" {
			t.Errorf("a proxy event carries another lane's discriminator: gateway=%q otel=%q",
				ev.GatewayRequestID, ev.OtelRequestID)
		}
	})
}

// TestAnUnsetLaneIsRefusedRatherThanDefaulted.
func TestAnUnsetLaneIsRefusedRatherThanDefaulted(t *testing.T) {
	_, err := EventsFor(Lane{}, sampleIdentity(), "req-1", sampleAt, sampleCaptured())
	if err == nil {
		t.Fatal("EventFor accepted a zero Lane; an unconfigured lane must be refused, never defaulted")
	}
	if !strings.Contains(err.Error(), "lane") {
		t.Errorf("error %q does not name the lane as the problem", err)
	}
}

// TestTheLanesAreDisjoint.
func TestTheLanesAreDisjoint(t *testing.T) {
	if LaneGateway.IDPrefix == LaneProxy.IDPrefix {
		t.Errorf("both lanes mint the prefix %q; a minted id would not say which lane produced it",
			LaneGateway.IDPrefix)
	}
	if LaneGateway.Name == LaneProxy.Name {
		t.Errorf("both lanes name the namespace %q; activity_ids would collide", LaneGateway.Name)
	}
	if LaneGateway.IDPrefix != GatewayIDPrefix {
		t.Errorf("LaneGateway.IDPrefix = %q, want the shipped %q", LaneGateway.IDPrefix, GatewayIDPrefix)
	}
}

// TestLaneNamesMatchTheActivityIDNamespaces crosses the module seam.
func TestLaneNamesMatchTheActivityIDNamespaces(t *testing.T) {
	for _, tc := range []struct {
		lane Lane
		want string
	}{
		{LaneGateway, ":gateway:req-1"},
		{LaneProxy, ":proxy:req-1"},
	} {
		var ids []string
		for _, ev := range mustPair(tc.lane, sampleIdentity(), "req-1", sampleAt, sampleCaptured()) {
			body := postThroughRealClient(t, ev, true)

			var p struct {
				ActivityID string `json:"activity_id"`
				SpanCount  int    `json:"span_count"`
				Spans      []any  `json:"spans"`
			}
			if err := json.Unmarshal(body, &p); err != nil {
				t.Fatalf("unmarshal posted payload: %v", err)
			}
			if !strings.HasSuffix(p.ActivityID, tc.want) {
				t.Errorf("lane %s %s produced activity_id %q, want it to end in %q",
					tc.lane.Name, ev.EventType, p.ActivityID, tc.want)
			}
			// No span, on either half. `llm_completion` IS the activity here, so a
			// span nested inside it would represent one call twice -- and core
			// discards embedded spans on the normal path anyway.
			if p.SpanCount != 0 || len(p.Spans) != 0 {
				t.Errorf("lane %s %s posted span_count=%d spans=%d, want none",
					tc.lane.Name, ev.EventType, p.SpanCount, len(p.Spans))
			}
			ids = append(ids, p.ActivityID)
		}
		// The two halves must land on ONE timeline row.
		if ids[0] != ids[1] {
			t.Errorf("lane %s split its pair across activity_ids %q and %q", tc.lane.Name, ids[0], ids[1])
		}
	}
}

// TestEmitRefusesAnUnconfiguredLane is the runtime half of the refusal: an
// Emitter with no Lane must drop the call loudly rather than file it under
// whichever lane happens to be first in the source.
func TestEmitRefusesAnUnconfiguredLane(t *testing.T) {
	delivery := newFakeDelivery()
	var warned bool
	em := &Emitter{
		Deliver: delivery.Deliver,
		DID:     func() string { return "did:openbox:dev" },
		Warn:    func(string, ...any) { warned = true },
	}
	em.Emit(context.Background(), capturedWithSession("sess-1"))

	if !warned {
		t.Error("an Emitter with no Lane emitted silently; a governance gap nobody is told about " +
			"is indistinguishable from a working lane")
	}
	if n := delivery.sessionsWithEvents(); n != 0 {
		t.Errorf("an Emitter with no Lane delivered %d session(s) worth of event(s); it must deliver none", n)
	}
}

// TestEmitFilesUnderTheConfiguredLane is the positive control for the above:
// the same emitter, with a lane set, does produce the event.
func TestEmitFilesUnderTheConfiguredLane(t *testing.T) {
	delivery := newFakeDelivery()
	em := &Emitter{
		Lane:    LaneProxy,
		Deliver: delivery.Deliver,
		DID:     func() string { return "did:openbox:dev" },
		Warn:    func(string, ...any) {},
		Elected: func() bool { return true },
	}
	em.Emit(context.Background(), capturedWithSession("sess-1"))

	if n := delivery.sessionsWithEvents(); n != 1 {
		t.Fatalf("delivered event(s) for %d session(s), want 1", n)
	}
}

// mustPair is EventsFor for tests that are not about the pairing itself. It
// asserts the shape every caller depends on: exactly two halves, opening first.
func mustPair(lane Lane, id Identity, requestID string, at time.Time, c gateway.Captured) []client.DevEvent {
	events, err := EventsFor(lane, id, requestID, at, c)
	if err != nil {
		panic(err)
	}
	if len(events) != 2 {
		panic(fmt.Sprintf("EventsFor returned %d events, want the Started/Completed pair", len(events)))
	}
	if events[0].EventType != client.EventTurnStarted || events[1].EventType != client.EventTurnCompleted {
		panic(fmt.Sprintf("EventsFor returned %s then %s, want TurnStarted then TurnCompleted; "+
			"the spool delivers in append order and core looks a completion's start up at ingest",
			events[0].EventType, events[1].EventType))
	}
	return events
}

// mustEvent is the COMPLETED half, which is what a test not about the pair
// means by "the event": it is the half that carries the status, the duration and
// the response.
func mustEvent(lane Lane, id Identity, requestID string, at time.Time, c gateway.Captured) client.DevEvent {
	return mustPair(lane, id, requestID, at, c)[1]
}

// mustStarted is the opening half.
func mustStarted(lane Lane, id Identity, requestID string, at time.Time, c gateway.Captured) client.DevEvent {
	return mustPair(lane, id, requestID, at, c)[0]
}

// onlyCompleted adapts the pair to a test that means the completed half.
func onlyCompleted(events []client.DevEvent, err error) (client.DevEvent, error) {
	if err != nil {
		return client.DevEvent{}, err
	}
	return events[len(events)-1], nil
}
