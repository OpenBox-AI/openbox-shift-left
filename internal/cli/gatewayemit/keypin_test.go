package gatewayemit

import (
	"strings"
	"testing"
)

// shippedGatewayEventID keypin_test.go; the shipped gateway idempotency key
// did not move.
const shippedGatewayEventID = "gw-70a0eb0b9ffb39fd58af125c32ffd3f7"

func TestGatewayEventIDIsUnchangedByTheLaneWork(t *testing.T) {
	// The COMPLETED half is the one that shipped, and the pin still holds for a
	// reason worth stating: the id hashes the event's timestamp, which is now the
	// relay's measured end-of-stream rather than the emit time. On a Captured the
	// relay never measured -- a refusal, an unreachable upstream, this fixture --
	// the bounds fall back to the emit time, so the shipped key is unmoved.
	ev := mustEvent(LaneGateway, sampleIdentity(), "req-1", sampleAt, sampleCaptured())
	if ev.EventID != shippedGatewayEventID {
		t.Errorf("gateway event_id = %q, want the shipped %q.\n"+
			"The lane work must not move this: a redelivered call presenting a different "+
			"idempotency key is counted twice by core.", ev.EventID, shippedGatewayEventID)
	}
}

// TestTheLaneNameIsNotHashed is the other half, and it is what makes the pin
// above achievable at all.
func TestTheLaneNameIsNotHashed(t *testing.T) {
	gw := mustEvent(LaneGateway, sampleIdentity(), "req-1", sampleAt, sampleCaptured())
	px := mustEvent(LaneProxy, sampleIdentity(), "req-1", sampleAt, sampleCaptured())

	if !strings.HasPrefix(px.EventID, ProxyIDPrefix) {
		t.Errorf("proxy event_id = %q, want the %q prefix; a proxy event's key must not read as "+
			"the gateway's in a log or in storage", px.EventID, ProxyIDPrefix)
	}
	if strings.TrimPrefix(gw.EventID, GatewayIDPrefix) != strings.TrimPrefix(px.EventID, ProxyIDPrefix) {
		t.Errorf("the two lanes hashed different inputs (gw=%q px=%q). Adding the lane to the hash "+
			"would move every shipped gateway key; the prefix alone is what distinguishes them.",
			gw.EventID, px.EventID)
	}
}

// TestTheTwoHalvesGetDistinctEventIDs asserted rather than assumed: the eventID
// hash deliberately EXCLUDES the lane name, so which of its parts separate the
// halves is not obvious from the call site. The event type is what does it.
func TestTheTwoHalvesGetDistinctEventIDs(t *testing.T) {
	pair := mustPair(LaneProxy, sampleIdentity(), "req-1", sampleAt, sampleCaptured())
	if pair[0].EventID == pair[1].EventID {
		t.Fatalf("both halves share event_id %q, so core's idempotency key absorbs one as a "+
			"duplicate of the other and the activity is left unpaired", pair[0].EventID)
	}
	for _, ev := range pair {
		if !strings.HasPrefix(ev.EventID, ProxyIDPrefix) {
			t.Errorf("%s event_id = %q, want the %q prefix", ev.EventType, ev.EventID, ProxyIDPrefix)
		}
	}
}
