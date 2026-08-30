package gatewayemit

import (
	"strings"
	"testing"
)

// shippedGatewayEventID keypin_test.go; the shipped gateway idempotency key
// did not move.
const shippedGatewayEventID = "gw-70a0eb0b9ffb39fd58af125c32ffd3f7"

func TestGatewayEventIDIsUnchangedByTheLaneWork(t *testing.T) {
	ev, err := EventFor(LaneGateway, sampleIdentity(), "req-1", sampleAt, sampleCaptured())
	if err != nil {
		t.Fatalf("EventFor: %v", err)
	}
	if ev.EventID != shippedGatewayEventID {
		t.Errorf("gateway event_id = %q, want the shipped %q.\n"+
			"The lane work must not move this: a redelivered call presenting a different "+
			"idempotency key is counted twice by core.", ev.EventID, shippedGatewayEventID)
	}
}

// TestTheLaneNameIsNotHashed is the other half, and it is what makes the pin
// above achievable at all.
func TestTheLaneNameIsNotHashed(t *testing.T) {
	gw, err := EventFor(LaneGateway, sampleIdentity(), "req-1", sampleAt, sampleCaptured())
	if err != nil {
		t.Fatalf("EventFor(gateway): %v", err)
	}
	px, err := EventFor(LaneProxy, sampleIdentity(), "req-1", sampleAt, sampleCaptured())
	if err != nil {
		t.Fatalf("EventFor(proxy): %v", err)
	}

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
