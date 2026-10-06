package gatewayemit

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// TestConcurrentCapturesEachSpoolBothHalves is a REGRESSION GUARD and NOT a
// reproduction. Concurrent captures are not what leaves an activity with only
// its Started half at the control plane; failed delivery is. So a green run
// here is not evidence about delivery loss, and it must not be read as such.
//
// What this test does own: Emit builds both halves at one call site and appends
// them in order, stopping if the first append fails. If concurrent appends could
// interleave, tear a line, or lose one, an activity would arrive single-sided --
// a property worth holding on its own terms.
func TestConcurrentCapturesEachSpoolBothHalves(t *testing.T) {
	const (
		session = "sess-concurrent"
		callers = 48
	)
	em, spool, warnings := newTestEmitter(t)

	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := capturedWithSession(session)
			// A distinct upstream request id per caller, because the activity id is
			// built from it. Without this these would be ONE activity emitted 48
			// times, which exercises idempotency and says nothing about pairing.
			c.ResponseHeaders = map[string]string{"Request-Id": fmt.Sprintf("req_upstream_%d", i)}
			em.Emit(context.Background(), c)
		}(i)
	}
	wg.Wait()

	events := spooledEvents(t, spool, session)
	if len(events) != 2*callers {
		t.Errorf("spooled %d events for %d concurrent captures, want %d",
			len(events), callers, 2*callers)
	}

	// Grouped per activity_id, never by parity. An EVEN count is satisfied just as
	// well by two single-sided activities as by one paired activity, which is the
	// error that made a live pairing check read as green while halves went
	// missing.
	type halves struct{ started, completed int }
	byActivity := map[string]*halves{}
	seenEventID := map[string]bool{}
	for _, ev := range events {
		// The triple as core assembles it: session, lane namespace, discriminator.
		key := ev.SessionID + ":" + LaneGateway.Name + ":" + ev.GatewayRequestID
		h := byActivity[key]
		if h == nil {
			h = &halves{}
			byActivity[key] = h
		}
		switch ev.EventType {
		case client.EventTurnStarted:
			h.started++
		case client.EventTurnCompleted:
			h.completed++
		default:
			t.Errorf("activity %s carries an unexpected event type %q", key, ev.EventType)
		}
		if seenEventID[ev.EventID] {
			t.Errorf("two spooled events share event_id %s, so the idempotency key would "+
				"collapse them into one row at ingest", ev.EventID)
		}
		seenEventID[ev.EventID] = true
	}

	if len(byActivity) != callers {
		t.Errorf("%d distinct activity ids for %d captures: concurrent callers collided on one",
			len(byActivity), callers)
	}
	for key, h := range byActivity {
		if h.started != 1 || h.completed != 1 {
			t.Errorf("activity %s spooled %d Started and %d Completed halves, want exactly one "+
				"of each: a single-sided activity is the defect this guards",
				key, h.started, h.completed)
		}
	}
	if w := warnings.String(); w != "" {
		t.Errorf("concurrent captures produced warnings, so something was dropped: %s", w)
	}
}
