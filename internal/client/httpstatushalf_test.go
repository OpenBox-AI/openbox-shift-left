package client

import "testing"

// `metadata.http_status` shipped on BOTH halves, and 116 of 116 live
// `ActivityStarted` rows asserted `200`. A Started row represents a request that
// has not been answered yet; a status code on it is a claim about a response that
// did not exist when the row was made. `docs/mapping.md:464,477` already said
// completed-only, so the code and the docs disagreed and NO test pinned either
// direction — which is why the disagreement survived.
//
// The cause was structural rather than a typo: gatewayemit builds both halves
// from one shared `span(stage)` closure (`internal/cli/gatewayemit/event.go:84-93`)
// that sets `HTTPStatus` unconditionally, and `buildMetadata` emitted it for any
// non-zero value. So the fix belongs here, in the one funnel every adapter passes
// through, rather than in the one adapter that happened to expose it.

// TestTheStartedHalfCarriesNoHTTPStatus is the direction that was wrong.
func TestTheStartedHalfCarriesNoHTTPStatus(t *testing.T) {
	for _, et := range []EventType{EventTurnStarted, EventToolCall} {
		t.Run(string(et), func(t *testing.T) {
			ev := modelCallEvent(et, "req", "")
			// Exactly what the shared closure does: the relay's observed status,
			// copied onto both halves.
			ev.Span.HTTPStatus = 200

			meta, ok := wireOf(t, ev)["metadata"].(map[string]any)
			if !ok {
				t.Fatal("no metadata on a started half")
			}
			if got, present := meta["http_status"]; present {
				t.Errorf("http_status = %v on an %s row; a Started row is an unanswered request, "+
					"so a status on it asserts a response that did not exist. 116/116 live rows "+
					"carried this.", got, et)
			}
		})
	}
}

// TestTheCompletedHalfStillCarriesHTTPStatus is the other direction, and it is
// half the point: removing a false assertion must not remove the true one. A 5xx
// and a call whose transport failed before any response existed would otherwise
// store identically to a success whose reply was not captured.
func TestTheCompletedHalfStillCarriesHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		et   EventType
		want int
	}{
		{EventTurnCompleted, 503},
		{EventToolResult, 200},
	} {
		t.Run(string(tc.et), func(t *testing.T) {
			ev := modelCallEvent(tc.et, "req", "resp")
			ev.Span.HTTPStatus = tc.want

			meta, ok := wireOf(t, ev)["metadata"].(map[string]any)
			if !ok {
				t.Fatal("no metadata on a completed half")
			}
			got, present := meta["http_status"]
			if !present {
				t.Fatalf("http_status is absent on an %s row; the relay observed %d and nothing "+
					"stores it. metadata=%v", tc.et, tc.want, meta)
			}
			if n, isNum := got.(float64); !isNum || int(n) != tc.want {
				t.Errorf("http_status = %v, want %d", got, tc.want)
			}
		})
	}
}

// TestAnExplicitMetadataStatusIsNotResurrectedOnTheStartedHalf the guard has to
// bound the KEY, not just the span field. buildMetadata copies caller metadata in
// first and only then fills from the span, so a gate that checked the span alone
// would let an adapter put the same false assertion back by hand.
func TestAnExplicitMetadataStatusIsNotResurrectedOnTheStartedHalf(t *testing.T) {
	ev := modelCallEvent(EventTurnStarted, "req", "")
	ev.Metadata = map[string]any{"http_status": 200}

	meta, ok := wireOf(t, ev)["metadata"].(map[string]any)
	if !ok {
		t.Fatal("no metadata on a started half")
	}
	if got, present := meta["http_status"]; present {
		t.Errorf("http_status = %v, set directly in metadata on a Started row; the rule is about "+
			"the row's meaning, so it cannot be satisfied by an adapter writing the key itself", got)
	}
}

// TestASignalRowCarriesNoHTTPStatus a signal is neither half of an activity, and
// none of the signal-bearing events observes an HTTP exchange.
func TestASignalRowCarriesNoHTTPStatus(t *testing.T) {
	ev := modelCallEvent(EventTurnStarted, "req", "")
	ev.EventType = EventAPIError
	ev.Span.HTTPStatus = 429

	meta, ok := wireOf(t, ev)["metadata"].(map[string]any)
	if !ok {
		t.Fatal("no metadata on a signal row")
	}
	if got, present := meta["http_status"]; present {
		t.Errorf("http_status = %v on a SignalReceived row", got)
	}
}
