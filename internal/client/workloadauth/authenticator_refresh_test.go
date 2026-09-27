package workloadauth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// primeAt fetches one token with the clock pinned at start and returns a
// setter that moves the clock.
func primeAt(t *testing.T, a *Authenticator, start time.Time) (string, func(time.Duration)) {
	t.Helper()
	now := start
	a.now = func() time.Time { return now }
	tok, _, err := a.Token(context.Background())
	if err != nil {
		t.Fatalf("priming Token: %v", err)
	}
	return tok, func(d time.Duration) { now = start.Add(d) }
}

func TestTokenInsideRefreshAheadWindowIsRenewed(t *testing.T) {
	srv := newCountingWorkloadServer(t)
	a := newTestAuthenticator(t, "", srv)
	tok1, at := primeAt(t, a, time.Now())

	at(200 * time.Second) // 100s left: inside refreshAhead, outside refreshSkew
	tok2, fromCache, err := a.Token(context.Background())
	if err != nil || fromCache {
		t.Fatalf("Token = fromCache %v, err %v; want a renewal", fromCache, err)
	}
	if tok2 == tok1 || srv.exchangeHits.Load() != 2 {
		t.Fatalf("exchange hits = %d, same token = %v; want a second exchange and a new token", srv.exchangeHits.Load(), tok2 == tok1)
	}
}

func TestTransientEarlyRenewalFailureKeepsTheUsableToken(t *testing.T) {
	srv := newCountingWorkloadServer(t)
	path := filepath.Join(t.TempDir(), "token.json")
	a := newTestAuthenticator(t, path, srv)
	tok1, at := primeAt(t, a, time.Now())

	srv.setExchange(http.StatusServiceUnavailable, false)
	at(200 * time.Second)
	tok, fromCache, err := a.Token(context.Background())
	if err != nil || !fromCache || tok != tok1 {
		t.Fatalf("Token = %q fromCache %v err %v; want the old token from cache", tok, fromCache, err)
	}

	// The backoff is on disk, so a second process does not try again.
	b := newTestAuthenticator(t, path, srv)
	b.now = a.now
	b.thumb = a.thumb
	b.key = a.key
	hits := srv.exchangeHits.Load()
	if tok, _, err := b.Token(context.Background()); err != nil || tok != tok1 {
		t.Fatalf("second process Token = %q, %v; want the old token", tok, err)
	}
	if srv.exchangeHits.Load() != hits {
		t.Fatal("a second process retried renewal inside the backoff")
	}

	// Once the backoff lapses, renewal is tried again.
	srv.setExchange(0, false)
	at(200*time.Second + refreshRetryBackoff)
	if tok, fromCache, err := a.Token(context.Background()); err != nil || fromCache || tok == tok1 {
		t.Fatalf("after backoff Token = %q fromCache %v err %v; want a renewed token", tok, fromCache, err)
	}
}

func TestHungEarlyRenewalIsBoundedAndFallsBack(t *testing.T) {
	srv := newCountingWorkloadServer(t)
	a := newTestAuthenticator(t, "", srv)
	a.renewTimeout = 100 * time.Millisecond
	tok1, at := primeAt(t, a, time.Now())

	srv.setExchange(0, true)
	at(200 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	began := time.Now()
	tok, fromCache, err := a.Token(ctx)
	if err != nil || !fromCache || tok != tok1 {
		t.Fatalf("Token = %q fromCache %v err %v; want the old token", tok, fromCache, err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("a hung renewal took %v; want it bounded by renewTimeout", took)
	}
}

func TestTokenPastRefreshSkewStillFailsWhenRenewalFails(t *testing.T) {
	srv := newCountingWorkloadServer(t)
	a := newTestAuthenticator(t, "", srv)
	_, at := primeAt(t, a, time.Now())

	srv.setExchange(http.StatusServiceUnavailable, false)
	at(280 * time.Second) // 20s left: never handed out
	if _, _, err := a.Token(context.Background()); err == nil {
		t.Fatal("Token succeeded with a token inside refreshSkew and a failing endpoint")
	}
}

func TestPermanentEarlyRenewalFailureIsNotMasked(t *testing.T) {
	srv := newCountingWorkloadServer(t)
	a := newTestAuthenticator(t, "", srv)
	_, at := primeAt(t, a, time.Now())

	srv.setExchange(http.StatusUnauthorized, false)
	at(200 * time.Second)
	if _, _, err := a.Token(context.Background()); err == nil {
		t.Fatal("a permanent exchange rejection fell back to the old token")
	}
}

func TestWarmRenewsOnlyInsideTheWindow(t *testing.T) {
	srv := newCountingWorkloadServer(t)
	a := newTestAuthenticator(t, filepath.Join(t.TempDir(), "token.json"), srv)
	_, at := primeAt(t, a, time.Now())

	at(60 * time.Second)
	if err := a.Warm(context.Background()); err != nil || srv.exchangeHits.Load() != 1 {
		t.Fatalf("Warm outside the window: err %v, exchanges %d; want a no-op", err, srv.exchangeHits.Load())
	}
	at(190 * time.Second)
	if err := a.Warm(context.Background()); err != nil || srv.exchangeHits.Load() != 2 {
		t.Fatalf("Warm inside the window: err %v, exchanges %d; want one renewal", err, srv.exchangeHits.Load())
	}
	// The renewed token is fresh for a hook with no network call.
	hits := srv.exchangeHits.Load()
	if _, fromCache, err := a.Token(context.Background()); err != nil || !fromCache || srv.exchangeHits.Load() != hits {
		t.Fatalf("Token after Warm: fromCache %v err %v", fromCache, err)
	}
}

func TestWarmFetchesAMissingToken(t *testing.T) {
	srv := newCountingWorkloadServer(t)
	a := newTestAuthenticator(t, filepath.Join(t.TempDir(), "token.json"), srv)
	if err := a.Warm(context.Background()); err != nil || srv.exchangeHits.Load() != 1 {
		t.Fatalf("Warm on an empty cache: err %v, exchanges %d", err, srv.exchangeHits.Load())
	}
}

func TestWarmLeavesANegativeEntryAlone(t *testing.T) {
	srv := newCountingWorkloadServer(t)
	srv.setBootstrapCode(http.StatusConflict)
	a := newTestAuthenticator(t, "", srv)
	_, _, _ = a.Token(context.Background())
	hits := srv.bootstrapHits.Load()
	if err := a.Warm(context.Background()); err != nil || srv.bootstrapHits.Load() != hits {
		t.Fatalf("Warm under a negative entry: err %v, bootstrap hits %d -> %d", err, hits, srv.bootstrapHits.Load())
	}
}

func TestNetCauseNamesTheFailureClass(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "timeout"},
		{&net.DNSError{Err: "no such host", Name: "identity.example"}, "dns lookup failed"},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "connection refused"},
		{&net.OpError{Op: "read", Err: syscall.ECONNRESET}, "connection reset"},
		{errors.New("something else"), "network error"},
	}
	for _, c := range cases {
		if got := netCause(c.err); got != c.want {
			t.Errorf("netCause(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestNetworkFaultCarriesItsCause(t *testing.T) {
	srv := newCountingWorkloadServer(t)
	srv.setExchange(0, true)
	a := newTestAuthenticator(t, "", srv)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err := a.Token(ctx)
	werr := asWorkloadError(t, err)
	if werr.Stage != StageExchange || werr.Reason != "network fault" || werr.Detail != "timeout" {
		t.Fatalf("err = %v; want an exchange network fault naming the timeout", err)
	}
}
