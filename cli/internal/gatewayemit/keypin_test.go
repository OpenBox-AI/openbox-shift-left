package gatewayemit

import (
	"strings"
	"testing"
)

// keypin_test.go — the shipped gateway idempotency key did not move.
//
// `eventID`'s prefix became lane-scoped so a proxy event's key does not read
// `gw-`. The gateway lane's key must be BYTE-IDENTICAL to what it emitted before
// that change: the spool can be drained by a different process long after the
// daemon that wrote it exited (INV-5), and a redelivery presenting a different
// key makes core count the same model call twice.
//
// The literal below was recomputed INDEPENDENTLY from the pre-change source —
// `sha256` over the same six fields with the same 0x1f separator, prefixed
// `gw-` — rather than copied from the new implementation's output. A pin taken
// from the code it is pinning proves nothing.
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
//
// Two lanes observing the same exchange produce the same HASH and differ only in
// the prefix. That is deliberate: hashing the lane would have moved every shipped
// gateway key. It is safe because the id is already lane-scoped — a fallback id
// carries its lane's own prefix, and a provider `Request-Id` belongs to one
// exchange that only one lane observed. Uniqueness comes from the request id, not
// from the hash input.
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
		t.Errorf("proxy event_id = %q, want the %q prefix — a proxy event's key must not read as "+
			"the gateway's in a log or in storage", px.EventID, ProxyIDPrefix)
	}
	if strings.TrimPrefix(gw.EventID, GatewayIDPrefix) != strings.TrimPrefix(px.EventID, ProxyIDPrefix) {
		t.Errorf("the two lanes hashed different inputs (gw=%q px=%q). Adding the lane to the hash "+
			"would move every shipped gateway key; the prefix alone is what distinguishes them.",
			gw.EventID, px.EventID)
	}
}
