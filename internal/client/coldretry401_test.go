package client

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"github.com/openbox-ai/openbox-shift-left/internal/client/workloadauth"
)

// cachedBearer and coldBearer are the two bearer values sequentialTokenFake
// hands out across a scripted Token() call sequence: cachedBearer for the
// first (fromCache=true) call, coldBearer for the forced-cold retry.
const (
	cachedBearer = "test-bearer-cached"
	coldBearer   = "test-bearer-cold"
)

// tokenResult is one scripted Token() call outcome for sequentialTokenFake.
type tokenResult struct {
	bearer    string
	fromCache bool
	err       error
}

// sequentialTokenFake is the tokens test seam (Config.tokens) for the 401
// cold-retry tests: unlike fakeTokenSource (client_test.go), which always
// returns the same fixed bearer, this fake answers a different scripted
// result on each successive Token() call -- which is exactly what post's one
// cold retry needs to exercise: a first (possibly cached) token, then a
// second, forced-cold one after Invalidate.
type sequentialTokenFake struct {
	mu          sync.Mutex
	script      []tokenResult
	calls       int
	invalidated int
}

func (f *sequentialTokenFake) Token(context.Context) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	if i >= len(f.script) {
		i = len(f.script) - 1 // repeat the last scripted result past the script's end
	}
	f.calls++
	r := f.script[i]
	return r.bearer, r.fromCache, r.err
}

func (f *sequentialTokenFake) Invalidate() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated++
	return nil
}

func (f *sequentialTokenFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *sequentialTokenFake) invalidateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.invalidated
}

// newSeqTestClient builds a Client wired to a sequentialTokenFake in place of
// a real workloadauth.Authenticator, against srv.
func newSeqTestClient(t *testing.T, srv *memhttptest.Server, tf *sequentialTokenFake) *Client {
	t.Helper()
	apiKey := testAPIKey
	c, err := New(Config{
		BaseURL:            srv.URL,
		APIKey:             apiKey,
		WorkloadPrivateKey: testWorkloadKey,
		tokens:             tf,
		RetryBase:          durPtr(time.Millisecond),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// countingTokenServer answers each POST to evaluatePath in sequence from
// statuses, echoing back the workload token header it saw so a test can
// assert which token backed which attempt; the last status repeats past the
// slice's end.
func countingTokenServer(t *testing.T, statuses ...int) (*memhttptest.Server, func() int, func() []string) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	var tokensSeen []string
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := calls
		calls++
		tokensSeen = append(tokensSeen, r.Header.Get(headerWorkloadToken))
		mu.Unlock()

		status := statuses[len(statuses)-1]
		if n < len(statuses) {
			status = statuses[n]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status >= 200 && status < 300 {
			_, _ = w.Write([]byte(`{"verdict":"allow"}`))
		} else {
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() int { mu.Lock(); defer mu.Unlock(); return calls },
		func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), tokensSeen...) }
}

// TestPost401ThenColdRetrySucceeds: a 401 on the first attempt earns exactly
// one further cold retry; when core accepts the freshly (re-)acquired token,
// Emit succeeds within the same call. This is the owner-decision reversal of
// the old "401 is never resent" rule: 3 parallel hooks each fetching a fresh
// token once saw core answer a spurious 401 (core maps any datastore error
// during agent lookup to 401) on the first, then 200 twice more moments later
// with an identical identity.
func TestPost401ThenColdRetrySucceeds(t *testing.T) {
	srv, calls, tokensSeen := countingTokenServer(t, http.StatusUnauthorized, http.StatusOK)
	tf := &sequentialTokenFake{script: []tokenResult{
		{bearer: cachedBearer, fromCache: true},
		{bearer: coldBearer, fromCache: false},
	}}
	c := newSeqTestClient(t, srv, tf)

	v, err := c.Emit(context.Background(), sampleEvent())
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if v.Verdict != VerdictAllow {
		t.Errorf("verdict = %q, want ALLOW", v.Verdict)
	}
	if got := calls(); got != 2 {
		t.Fatalf("POST count = %d, want exactly 2 (the original attempt + one cold retry)", got)
	}
	if got := tf.callCount(); got != 2 {
		t.Errorf("Token() calls = %d, want 2", got)
	}
	// Two calls, not one: attempt() itself invalidates a cached token's 401
	// (fromCache=true here), and post's cold-retry step invalidates
	// unconditionally before its one extra attempt. Both are harmless (a
	// second Invalidate on an already-cold cache is a no-op for the real
	// Authenticator); what matters is the second Token() call goes cold.
	if got := tf.invalidateCount(); got != 2 {
		t.Errorf("Invalidate() calls = %d, want 2", got)
	}
	seen := tokensSeen()
	if len(seen) != 2 || seen[0] != cachedBearer || seen[1] != coldBearer {
		t.Errorf("tokens seen = %v, want [%s %s] (the retry must use the second, cold token)", seen, cachedBearer, coldBearer)
	}
}

// TestPost401ThenColdRetryStillFails: when the second, cold attempt also gets
// a 401, Emit still classifies "401" (never ErrRefused: the two 401s are
// indistinguishable from a datastore hiccup mid-lookup) and costs exactly two
// POSTs -- the one cold retry, not an unbounded resend.
func TestPost401ThenColdRetryStillFails(t *testing.T) {
	srv, calls, _ := countingTokenServer(t, http.StatusUnauthorized, http.StatusUnauthorized)
	tf := &sequentialTokenFake{script: []tokenResult{
		{bearer: cachedBearer, fromCache: true},
		{bearer: coldBearer, fromCache: false},
	}}
	c := newSeqTestClient(t, srv, tf)

	_, err := c.Emit(context.Background(), sampleEvent())
	if err == nil {
		t.Fatal("Emit succeeded despite two consecutive 401s")
	}
	if !errors.Is(err, ErrDelivery) {
		t.Errorf("err = %v, want ErrDelivery", err)
	}
	if errors.Is(err, ErrRefused) {
		t.Error("a 401, even after its one cold retry, must never be a refusal")
	}
	if got := FailureClass(err); got != "401" {
		t.Errorf("FailureClass = %q, want %q", got, "401")
	}
	if got := calls(); got != 2 {
		t.Fatalf("POST count = %d, want exactly 2 (no further resend past the one cold retry)", got)
	}
}

// TestPost401ColdRetrySkippedWhenCtxAlreadyCanceled: once the caller's own
// context has expired, the cold retry is skipped -- it could only reproduce
// the same "context canceled" outcome attempt's own ctx handling already
// covers, at the cost of an extra Invalidate and token fetch for nothing.
func TestPost401ColdRetrySkippedWhenCtxAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var callCount int
	var mu sync.Mutex
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		// Cancel the caller's context before the response is flushed, so by the
		// time post's backoff loop returns control, ctx.Err() is already non-nil.
		cancel()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	t.Cleanup(srv.Close)

	tf := &sequentialTokenFake{script: []tokenResult{
		{bearer: cachedBearer, fromCache: true},
		{bearer: coldBearer, fromCache: false},
	}}
	c := newSeqTestClient(t, srv, tf)

	_, err := c.Emit(ctx, sampleEvent())
	if err == nil {
		t.Fatal("Emit succeeded despite a 401")
	}
	if !errors.Is(err, ErrDelivery) {
		t.Errorf("err = %v, want ErrDelivery", err)
	}
	mu.Lock()
	got := callCount
	mu.Unlock()
	if got != 1 {
		t.Errorf("POST count = %d, want exactly 1 (the cold retry must be skipped once ctx is already canceled)", got)
	}
	if got := tf.callCount(); got != 1 {
		t.Errorf("Token() calls = %d, want 1 (no cold-retry token fetch)", got)
	}
}

// TestPost401ColdRetryTokenFetchFails: when the retry's own Token() call
// fails (a bootstrap/exchange fault, not a judgment on the event), that
// *workloadauth.Error is returned verbatim -- never wrapped in an *httpError
// -- so isRefusal still reports false: the control plane never even saw this
// second attempt.
func TestPost401ColdRetryTokenFetchFails(t *testing.T) {
	srv, calls, _ := countingTokenServer(t, http.StatusUnauthorized)
	wantErr := &workloadauth.Error{Stage: workloadauth.StageExchange, Reason: "network fault", Detail: "timeout"}
	tf := &sequentialTokenFake{script: []tokenResult{
		{bearer: cachedBearer, fromCache: true},
		{err: wantErr},
	}}
	c := newSeqTestClient(t, srv, tf)

	_, err := c.Emit(context.Background(), sampleEvent())
	if err == nil {
		t.Fatal("Emit succeeded despite a 401 and a failed cold-retry token fetch")
	}
	if !errors.Is(err, ErrDelivery) {
		t.Errorf("err = %v, want ErrDelivery", err)
	}
	if errors.Is(err, ErrRefused) {
		t.Error("a token-acquisition failure on the cold retry must never be a refusal")
	}
	var werr *workloadauth.Error
	if !errors.As(err, &werr) {
		t.Fatalf("err does not wrap *workloadauth.Error: %v", err)
	}
	if werr != wantErr {
		t.Errorf("wrapped *workloadauth.Error = %+v, want the exact retry failure %+v", werr, wantErr)
	}
	var he *httpError
	if errors.As(err, &he) {
		t.Errorf("err must not also wrap an *httpError once the retry's own token fetch failed; got %v", he)
	}
	if got := calls(); got != 1 {
		t.Errorf("POST count = %d, want exactly 1 (the retry's token fetch failed before reaching the wire)", got)
	}
	if got := tf.callCount(); got != 2 {
		t.Errorf("Token() calls = %d, want 2 (the original call + the failed cold-retry call)", got)
	}
}
