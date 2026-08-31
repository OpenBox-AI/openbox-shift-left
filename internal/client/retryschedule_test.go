package client

import (
	"context"
	"errors"
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
// thing that blocks is post's own backoff wait.
type alwaysRetryable struct {
	status     int
	retryAfter string // Retry-After header value; empty ⇒ header absent
	calls      *int
	at         *[]time.Duration
	start      time.Time
}

func (rt alwaysRetryable) RoundTrip(r *http.Request) (*http.Response, error) {
	// http.Transport refuses a request whose context is already done, and a stub
	// that did not would make "no request after cancellation" unassertable.
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	*rt.calls++
	*rt.at = append(*rt.at, time.Since(rt.start))
	h := make(http.Header)
	if rt.retryAfter != "" {
		h.Set("Retry-After", rt.retryAfter)
	}
	return &http.Response{
		StatusCode: rt.status,
		Body:       io.NopCloser(strings.NewReader(`{"error":"try later"}`)),
		Header:     h,
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

func retryHarness(t *testing.T, status, retryAfter string) (*Client, *int, *[]time.Duration) {
	t.Helper()
	calls := 0
	at := []time.Duration{}
	code := http.StatusServiceUnavailable
	if status == "429" {
		code = http.StatusTooManyRequests
	}
	return scheduleClient(t, alwaysRetryable{
		status: code, retryAfter: retryAfter, calls: &calls, at: &at, start: time.Now(),
	}), &calls, &at
}

// TestRetryStaysInsideTheBudgetTheLinearRampSpent. The schedule this replaced
// was `time.After(attempt * retryBase)` -- 150ms then 300ms, three attempts,
// 450ms total, and identical in every hook process on every concurrent session.
// A control plane coming back from an outage met the whole fleet in lockstep.
//
// The replacement is exponential with full jitter, and the property that must
// not move is the ceiling: retryBudget is defined as exactly what the old ramp
// would have spent, sum(i*retryBase), so this asserts the new schedule cannot
// cost the developer more than the old one did.
//
// (The plan budgeted 900ms here, counting a third wait the loop never took.
// The bound is 450ms.)
func TestRetryStaysInsideTheBudgetTheLinearRampSpent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, calls, at := retryHarness(t, "503", "")

		start := time.Now()
		if _, err := c.post(t.Context(), evaluatePath, []byte(`{}`), "idem-1"); err == nil {
			t.Fatal("post returned nil error against a server that only 503s")
		}
		elapsed := time.Since(start)

		const wantBudget = 3 * defaultRetryBase // 450ms: 150 + 300
		if got := c.retryBudget(); got != wantBudget {
			t.Errorf("retryBudget() = %v, want %v (what the linear ramp spent)", got, wantBudget)
		}
		if elapsed > wantBudget {
			t.Errorf("post spent %v, over the %v the ramp it replaced would have spent. A hook that "+
				"blocks longer hurts the developer more than a lost event does", elapsed, wantBudget)
		}
		if *calls < 1 || *calls > defaultMaxRetries+1 {
			t.Errorf("attempts = %d, want between 1 and %d", *calls, defaultMaxRetries+1)
		}
		for i, d := range *at {
			if d > wantBudget {
				t.Errorf("attempt %d began at %v, past the whole budget %v", i, d, wantBudget)
			}
		}
	})
}

// TestRetryDelaysAreJittered is the point of the change and cannot be asserted
// by watching one run: two fleets retrying on the same deterministic schedule
// synchronise, and synchronised load is what stops a recovering control plane
// from recovering. Distinct sequences across runs are the evidence that the
// delays are drawn rather than computed.
func TestRetryDelaysAreJittered(t *testing.T) {
	seen := map[string]int{}
	for range 40 {
		synctest.Test(t, func(t *testing.T) {
			c, _, at := retryHarness(t, "503", "")
			_, _ = c.post(t.Context(), evaluatePath, []byte(`{}`), "idem-j")
			var b strings.Builder
			for _, d := range *at {
				b.WriteString(d.String())
				b.WriteByte(' ')
			}
			seen[b.String()]++
		})
	}
	if len(seen) < 2 {
		t.Errorf("40 runs produced %d distinct delay sequence(s): %v\nthe schedule is still "+
			"deterministic, so every hook in the fleet still retries in lockstep", len(seen), seen)
	}
}

// TestRetryAfterBeyondTheBudgetStopsRatherThanSleeps. Retry-After appeared
// nowhere in this repo, so a 429 asking for a minute was retried 150ms later.
// It is honoured now -- as a stop signal. Sleeping it out inline would hold a
// tool call open for as long as the server asked (INV-3), and would buy
// nothing: ErrDelivery re-spools the event and the next flush delivers it.
// Refusing to send again is the half of Retry-After that protects the server,
// and it is the half that is kept.
func TestRetryAfterBeyondTheBudgetStopsRatherThanSleeps(t *testing.T) {
	for _, header := range []string{"600", "5", "60"} {
		t.Run("Retry-After: "+header, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, calls, _ := retryHarness(t, "429", header)

				start := time.Now()
				_, err := c.post(t.Context(), evaluatePath, []byte(`{}`), "idem-ra")
				elapsed := time.Since(start)

				if err == nil {
					t.Fatal("post succeeded against a server that only 429s")
				}
				if *calls != 1 {
					t.Errorf("attempts = %d, want 1: the server said do not come back yet", *calls)
				}
				if elapsed > c.retryBudget() {
					t.Errorf("post slept %v honouring Retry-After: %s. The budget is %v and the "+
						"header is attacker-influenceable if the control plane is impersonated, so it "+
						"is also the DoS bound", elapsed, header, c.retryBudget())
				}
			})
		})
	}
}

// TestRetryAfterInsideTheBudgetIsWaitedOut is the other side: a server asking
// for a pause it can actually have gets it, and the attempt happens after.
func TestRetryAfterInsideTheBudgetIsWaitedOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, calls, at := retryHarness(t, "429", "")
		c.retryBase = time.Second // budget becomes 3s, comfortably over the 1s below
		c.http = &http.Client{Transport: alwaysRetryable{
			status: http.StatusTooManyRequests, retryAfter: "1", calls: calls, at: at, start: time.Now(),
		}}

		if _, err := c.post(t.Context(), evaluatePath, []byte(`{}`), "idem-ra2"); err == nil {
			t.Fatal("post succeeded against a server that only 429s")
		}
		if *calls < 2 {
			t.Fatalf("attempts = %d, want at least 2: a one-second pause fits inside a %v budget",
				*calls, c.retryBudget())
		}
		if got := (*at)[1]; got != time.Second {
			t.Errorf("second attempt began at %v, want exactly 1s: Retry-After replaces the computed "+
				"delay rather than being added to it", got)
		}
	})
}

// TestRetryStopsImmediatelyOnAnUnretryableStatus: a 4xx that is not 429 must
// cost zero waiting. Under a real clock this is indistinguishable from a fast
// machine.
func TestRetryStopsImmediatelyOnAnUnretryableStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		at := []time.Duration{}
		c := scheduleClient(t, alwaysRetryable{
			status: http.StatusBadRequest, calls: &calls, at: &at, start: time.Now(),
		})

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

// TestRetryAbortsOnContextCancel. The hook path depends on cancellation
// winning: INV-3 says a hook must never hold the developer's tool call open,
// and the enforcement caller cancels at its own evaluation budget.
//
// The attempt count is deliberately not asserted. The delays are drawn now, so
// whether a cancel scheduled a fixed distance out lands before or after the next
// attempt is a coin toss -- asserting it would be asserting the coin. What holds
// either way is the contract: the error is a cancellation, and post returns
// inside the budget rather than sitting out a full delay after being cancelled.
func TestRetryAbortsOnContextCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, calls, _ := retryHarness(t, "503", "")

		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			time.Sleep(defaultRetryBase / 8)
			cancel()
		}()

		start := time.Now()
		_, err := c.post(ctx, evaluatePath, []byte(`{}`), "idem-3")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("post error = %v, want context.Canceled", err)
		}
		if d := time.Since(start); d > c.retryBudget() {
			t.Errorf("post returned after %v, past the whole budget %v", d, c.retryBudget())
		}
		if *calls > defaultMaxRetries+1 {
			t.Errorf("attempts = %d, want at most %d", *calls, defaultMaxRetries+1)
		}
	})
}

// TestRetryMakesNoRequestOnAnAlreadyCancelledContext is the deterministic half
// of the same contract: an enforcement caller whose budget has already expired
// must not put a request on the wire on its way out.
func TestRetryMakesNoRequestOnAnAlreadyCancelledContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, calls, _ := retryHarness(t, "503", "")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		if _, err := c.post(ctx, evaluatePath, []byte(`{}`), "idem-4"); !errors.Is(err, context.Canceled) {
			t.Errorf("post error = %v, want context.Canceled", err)
		}
		if *calls != 0 {
			t.Errorf("the transport was reached %d time(s) under an already-cancelled context", *calls)
		}
	})
}

// TestRetryAfterParsesBothFormsRFC9110Allows. delta-seconds and HTTP-date are
// both legal, and a server picking the date form must not read as "no header
// at all" -- which is what an Atoi-only parse would have made it.
func TestRetryAfterParsesBothForms(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		header string
		want   time.Duration
		ok     bool
	}{
		{"5", 5 * time.Second, true},
		{"  5  ", 5 * time.Second, true},
		{"Mon, 31 Aug 2026 12:00:30 GMT", 30 * time.Second, true},
		{"Monday, 31-Aug-26 12:00:30 GMT", 30 * time.Second, true}, // RFC 850
		{"Mon Aug 31 12:00:30 2026", 30 * time.Second, true},       // ANSI C asctime
		{"", 0, false},
		{"0", 0, false},
		{"-3", 0, false},
		{"soon", 0, false},
		{"Mon, 31 Aug 2026 11:59:30 GMT", 0, false}, // already past
	} {
		got, ok := parseRetryAfter(tc.header, now)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseRetryAfter(%q) = (%v, %v), want (%v, %v)", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}
