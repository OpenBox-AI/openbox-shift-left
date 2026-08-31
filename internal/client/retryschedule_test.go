package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// alwaysRetryable answers every request with the same retryable status and no
// network at all. A RoundTripper rather than a memhttptest server because the
// schedule assertions below run inside a synctest bubble: everything that
// blocks has to block on something the bubble owns, and this way the only
// thing that blocks is post's own time.After.
type alwaysRetryable struct {
	status int
	calls  *int
	at     *[]time.Duration
	start  time.Time
}

func (rt alwaysRetryable) RoundTrip(r *http.Request) (*http.Response, error) {
	*rt.calls++
	*rt.at = append(*rt.at, time.Since(rt.start))
	return &http.Response{
		StatusCode: rt.status,
		Body:       io.NopCloser(strings.NewReader(`{"error":"try later"}`)),
		Header:     make(http.Header),
		Request:    r,
	}, nil
}

// scheduleClient borrows the package's own constructor and then puts back the
// two things it deliberately changes for speed: newTestClient shortens
// retryBase to 1ms so real-clock retry tests stay quick, and that is precisely
// the value under assertion here.
func scheduleClient(t *testing.T, rt http.RoundTripper) *Client {
	t.Helper()
	c, _ := newTestClient(t, "https://core.example", false)
	c.http = &http.Client{Transport: rt}
	c.retryBase = defaultRetryBase
	c.maxRetries = defaultMaxRetries
	return c
}

// TestRetryScheduleIsWhatItIsToday pins the delays post() actually waits,
// against a clock that costs nothing to advance. It asserts the schedule, not
// the outcome: a test that only checks "three attempts happened" cannot tell a
// 150/300 linear ramp from an exponential one, so replacing the ramp would be
// an unverified swap rather than a reviewed diff.
//
// Recorded because the plan's own arithmetic was wrong. It budgeted
// 150+300+450 = 900ms at the default MaxRetries; the loop is
// `for attempt := 0; attempt <= maxRetries` with maxRetries = 2, so it waits
// 1*base and 2*base and stops -- three attempts, 450ms. Anything that treats
// 900ms as "today's budget" doubles it.
func TestRetryScheduleIsWhatItIsToday(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		var at []time.Duration
		c := scheduleClient(t, alwaysRetryable{status: http.StatusServiceUnavailable, calls: &calls, at: &at, start: time.Now()})

		start := time.Now()
		if _, err := c.post(t.Context(), evaluatePath, []byte(`{}`), "idem-1"); err == nil {
			t.Fatal("post returned nil error against a server that only 503s")
		}
		elapsed := time.Since(start)

		if calls != defaultMaxRetries+1 {
			t.Errorf("attempts = %d, want %d (one initial + %d retries)", calls, defaultMaxRetries+1, defaultMaxRetries)
		}
		// Cumulative, not per-delay: the waits are 1*base then 2*base, so the
		// attempts land at 0, base, 3*base.
		want := []time.Duration{0, defaultRetryBase, 3 * defaultRetryBase}
		if got := fmt.Sprint(at); got != fmt.Sprint(want) {
			t.Errorf("attempt times = %v, want %v: the delay is linear in the attempt number "+
				"(150ms, then 300ms), unjittered and uncapped", at, want)
		}
		if elapsed != 3*defaultRetryBase {
			t.Errorf("total retry time = %v, want %v (150ms + 300ms). This is the budget Phase 05 "+
				"must not exceed; the plan's 900ms figure counts a third wait that never happens", elapsed, 3*defaultRetryBase)
		}
	})
}

// TestRetryStopsImmediatelyOnAnUnretryableStatus is the other half of the
// schedule: a 4xx that is not 429 must cost zero waiting. Under a real clock
// this is indistinguishable from a fast machine.
func TestRetryStopsImmediatelyOnAnUnretryableStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		var at []time.Duration
		c := scheduleClient(t, alwaysRetryable{status: http.StatusBadRequest, calls: &calls, at: &at, start: time.Now()})

		start := time.Now()
		if _, err := c.post(t.Context(), evaluatePath, []byte(`{}`), "idem-2"); err == nil {
			t.Fatal("post returned nil error against a 400")
		}
		if calls != 1 {
			t.Errorf("attempts = %d on a 400, want 1: a bad request does not become good by repeating", calls)
		}
		if d := time.Since(start); d != 0 {
			t.Errorf("waited %v before giving up on a 400, want 0", d)
		}
	})
}

// TestRetryAbortsMidBackoffOnContextCancel. The wait is a select on ctx.Done()
// and a timer, and the hook path depends on the cancel arm winning: INV-3 says
// a hook must never hold the developer's tool call open. Under a real clock a
// 150ms wait is too short to interrupt reliably; under a fake one the
// cancellation lands at an exact instant.
func TestRetryAbortsMidBackoffOnContextCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		var at []time.Duration
		c := scheduleClient(t, alwaysRetryable{status: http.StatusServiceUnavailable, calls: &calls, at: &at, start: time.Now()})

		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			time.Sleep(defaultRetryBase / 2) // mid-way through the first backoff
			cancel()
		}()

		start := time.Now()
		_, err := c.post(ctx, evaluatePath, []byte(`{}`), "idem-3")
		if err != context.Canceled {
			t.Errorf("post error = %v, want context.Canceled: the cancel arm of the backoff select must win", err)
		}
		if d := time.Since(start); d != defaultRetryBase/2 {
			t.Errorf("post returned after %v, want %v: it waited out the backoff instead of aborting", d, defaultRetryBase/2)
		}
		if calls != 1 {
			t.Errorf("attempts = %d, want 1: no attempt may start after cancellation", calls)
		}
	})
}
