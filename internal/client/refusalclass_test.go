package client

import (
	"context"
	"errors"
	"testing"
)

// TestEmit_OnlyAProvenRefusalCarriesErrRefused pins the classification at the
// wire, because that is where it is decided: the status code is the only thing
// the client can judge, and whether ErrRefused rides along is what decides
// whether a spooled event spends one of its five delivery attempts.
//
// Every case must still carry ErrDelivery -- a caller holding the only durable
// copy re-queues on that, and none of these are a reason to drop the event.
func TestEmit_OnlyAProvenRefusalCarriesErrRefused(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		refused bool
	}{
		{"400 the event itself is bad", 400, `{"code":400,"message":"invalid event_type: ToolCall"}`, true},
		{"403 forbidden", 403, `{"code":403,"message":"forbidden"}`, true},
		{"404 no such agent", 404, `{"code":404,"message":"agent not found"}`, true},
		{"422 unprocessable", 422, `{"code":422,"message":"unprocessable entity"}`, true},

		// 401 is the policy call, and it is not a hedge. Core answers "invalid
		// token or agent identity" both for a genuinely rejected identity and for
		// ANY datastore failure while looking the token up, so the two are
		// indistinguishable here. Spending attempts on it would let a database
		// blip delete governance evidence after five sweeps; a truly revoked
		// identity instead costs one cheap request per event per sweep, bounded by
		// the retention age.
		{"401 identity OR a datastore fault", 401, `{"code":401,"message":"invalid token or agent identity"}`, false},

		{"429 retryable, merely exhausted", 429, `{"code":429,"message":"slow down"}`, false},
		{"500 server fault", 500, `{"code":500,"message":"boom"}`, false},
		{"503 unavailable", 503, `{"code":503,"message":"unavailable"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fixedRespServer(t, tc.status, tc.body)
			c, _ := newTestClient(t, srv.URL, false)

			_, err := c.Emit(context.Background(), sampleEvent())
			if !errors.Is(err, ErrDelivery) {
				t.Fatalf("every failure stays ErrDelivery so the spool re-queues it; got %v", err)
			}
			if got := errors.Is(err, ErrRefused); got != tc.refused {
				t.Errorf("ErrRefused = %v, want %v for HTTP %d; this decides whether the event "+
					"spends a delivery attempt", got, tc.refused, tc.status)
			}
		})
	}
}

// TestEmit_ATransportFaultIsNotARefusal closes the other half of the
// classification: there is no status code at all, so nothing judged the event.
// A laptop that lost its network must not spend an event's attempts.
func TestEmit_ATransportFaultIsNotARefusal(t *testing.T) {
	// Nothing listens on port 1; the dial fails before any HTTP exchange.
	c, _ := newTestClient(t, "http://127.0.0.1:1", false)

	_, err := c.Emit(context.Background(), sampleEvent())
	if !errors.Is(err, ErrDelivery) {
		t.Fatalf("a transport fault must stay ErrDelivery so the spool re-queues it; got %v", err)
	}
	if errors.Is(err, ErrRefused) {
		t.Error("a transport fault was scored as a refusal; nothing judged the event, " +
			"so it must not spend a delivery attempt")
	}
}
