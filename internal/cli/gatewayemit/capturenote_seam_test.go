package gatewayemit

import "testing"

// TestEventsForCarriesTheResponseCompletenessFieldsOntoTheSpan pins the seam
// that carries the response-completeness note from the gateway onto the wire.
//
// Why this exists: the gateway computes ResponseBytesSeen/ResponseTruncated and
// payload.go turns them into activity_output.openbox_capture, but the two ends
// are joined only by this span() copy. Each end is covered by its own
// package's tests, but without this one deleting the two assignments would
// leave the suite green while the note silently vanished from every in-path
// row. Asserting a struct is not asserting the wire.
//
// The sibling for the four attribution fields is
// TestEventsForCopiesAttributionOntoBothHalves, which already covered its own
// copy through this same closure.
func TestEventsForCarriesTheResponseCompletenessFieldsOntoTheSpan(t *testing.T) {
	c := sampleCaptured()
	c.ResponseBytesSeen = 300 * 1024
	c.ResponseTruncated = true

	pair := mustPair(LaneGateway, sampleIdentity(), "req-capture-note", sampleAt, c)
	for _, ev := range pair {
		s := ev.Span
		if s == nil {
			t.Fatalf("%s: no span", ev.EventType)
		}
		// original_bytes must be the count the sink SAW, not what it stored --
		// that is the whole point of the counter, so a stored-length value here
		// would be indistinguishable from the bug it replaced.
		if s.ResponseBytesSeen != 300*1024 {
			t.Errorf("%s: ResponseBytesSeen = %d, want the offered byte count 307200",
				ev.EventType, s.ResponseBytesSeen)
		}
		if !s.ResponseTruncated {
			t.Errorf("%s: ResponseTruncated = false, want true", ev.EventType)
		}
	}
}

// TestEventsForLeavesTheCompletenessFieldsZeroWhenNothingWasCut is the other
// half: a complete response must carry truncated=false honestly rather than
// inheriting a stale true.
func TestEventsForLeavesTheCompletenessFieldsZeroWhenNothingWasCut(t *testing.T) {
	c := sampleCaptured()
	c.ResponseTruncated = false

	for _, ev := range mustPair(LaneGateway, sampleIdentity(), "req-complete", sampleAt, c) {
		if ev.Span != nil && ev.Span.ResponseTruncated {
			t.Errorf("%s: ResponseTruncated = true for an uncut response", ev.EventType)
		}
	}
}
