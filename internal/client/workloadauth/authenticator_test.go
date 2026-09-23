package workloadauth

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
)

// countingWorkloadServer serves a canonical bootstrap document and a fixed
// access token, counting how many times each endpoint is hit. status
// overrides the bootstrap response when non-zero (for negative-cache tests).
type countingWorkloadServer struct {
	srv           *memhttptest.Server
	bootstrapHits atomic.Int64
	exchangeHits  atomic.Int64

	mu            sync.Mutex
	bootstrapCode int
}

func newCountingWorkloadServer(t *testing.T) *countingWorkloadServer {
	c := &countingWorkloadServer{bootstrapCode: http.StatusOK}
	c.srv = memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/bootstrap"):
			c.bootstrapHits.Add(1)
			c.mu.Lock()
			code := c.bootstrapCode
			c.mu.Unlock()
			doc := validBootstrapDoc()
			doc["issuer"] = c.srv.URL + "/realms/openbox"
			doc["token_endpoint"] = c.srv.URL + "/realms/openbox/protocol/openid-connect/token"
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			if code != http.StatusOK {
				_, _ = w.Write([]byte(`{"reason_code":"workload_identity_unavailable"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(doc)
		case strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			c.exchangeHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "atk-" + strconv.FormatInt(c.exchangeHits.Load(), 10),
				"token_type":   "Bearer",
				"expires_in":   300,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	return c
}

func (c *countingWorkloadServer) setBootstrapCode(code int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bootstrapCode = code
}

func newTestAuthenticator(t *testing.T, cachePath string, srv *countingWorkloadServer) *Authenticator {
	t.Helper()
	key := testRSAKey(t)
	encoded, err := EncodePrivateKey(key)
	if err != nil {
		t.Fatalf("EncodePrivateKey: %v", err)
	}
	a, err := NewAuthenticator("obx_testkey", encoded, cachePath, srv.srv.URL, srv.srv.Client())
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	return a
}

func TestColdTokenBootstrapsAndExchangesOnce(t *testing.T) {
	srv := newCountingWorkloadServer(t)
	a := newTestAuthenticator(t, "", srv)

	tok, fromCache, err := a.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if fromCache {
		t.Fatal("expected a cold call, fromCache=false")
	}
	if tok == "" {
		t.Fatal("expected a non-empty token")
	}
	if srv.bootstrapHits.Load() != 1 || srv.exchangeHits.Load() != 1 {
		t.Fatalf("bootstrap=%d exchange=%d, want 1 and 1", srv.bootstrapHits.Load(), srv.exchangeHits.Load())
	}
}

func TestExpiredEntryDoesBootstrapAgain(t *testing.T) {
	srv := newCountingWorkloadServer(t)
	a := newTestAuthenticator(t, "", srv)

	fakeNow := time.Now()
	a.now = func() time.Time { return fakeNow }

	tok1, fromCache, err := a.Token(context.Background())
	if err != nil || fromCache {
		t.Fatalf("first Token: tok=%q fromCache=%v err=%v", tok1, fromCache, err)
	}

	// Advance past the document/token's shared lifetime: a cold process
	// after expiry makes one bootstrap and one exchange, never a
	// refresh-only path, because there is no separate bootstrap cache.
	fakeNow = fakeNow.Add(400 * time.Second)

	tok2, fromCache, err := a.Token(context.Background())
	if err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if fromCache {
		t.Fatal("expected the expired entry to force a cold call")
	}
	if tok1 == tok2 {
		t.Fatal("expected a fresh token after expiry")
	}
	if srv.bootstrapHits.Load() != 2 || srv.exchangeHits.Load() != 2 {
		t.Fatalf("bootstrap=%d exchange=%d, want 2 and 2", srv.bootstrapHits.Load(), srv.exchangeHits.Load())
	}
}

func TestWarmTokenServesFromCacheWithoutRequests(t *testing.T) {
	srv := newCountingWorkloadServer(t)
	a := newTestAuthenticator(t, "", srv)

	if _, _, err := a.Token(context.Background()); err != nil {
		t.Fatalf("priming Token: %v", err)
	}
	if _, _, err := a.Token(context.Background()); err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if srv.bootstrapHits.Load() != 1 || srv.exchangeHits.Load() != 1 {
		t.Fatalf("bootstrap=%d exchange=%d, want exactly 1 each across two calls", srv.bootstrapHits.Load(), srv.exchangeHits.Load())
	}
}

func TestInvalidateDeletesTheFile(t *testing.T) {
	srv := newCountingWorkloadServer(t)
	path := filepath.Join(t.TempDir(), "token.json")
	a := newTestAuthenticator(t, path, srv)

	if _, _, err := a.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	fc, ok := a.cache.(*FileCache)
	if !ok {
		t.Fatalf("cache = %T, want *FileCache for a non-empty path", a.cache)
	}
	if _, ok := fc.Load(); !ok {
		t.Fatal("expected the cache file to exist before Invalidate")
	}

	if err := a.Invalidate(); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if _, ok := fc.Load(); ok {
		t.Fatal("expected the cache file to be gone after Invalidate")
	}
}

func TestConcurrentColdCallsBootstrapOnce(t *testing.T) {
	srv := newCountingWorkloadServer(t)
	a := newTestAuthenticator(t, "", srv)

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	toks := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, _, err := a.Token(context.Background())
			toks[i] = tok
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		if toks[i] == "" {
			t.Fatalf("goroutine %d: empty token", i)
		}
	}
	if got := srv.bootstrapHits.Load(); got != 1 {
		t.Errorf("bootstrap hits = %d, want 1", got)
	}
	if got := srv.exchangeHits.Load(); got != 1 {
		t.Errorf("exchange hits = %d, want 1", got)
	}
}

func TestUnavailableIdentityIsNegativelyCachedFor60s(t *testing.T) {
	srv := newCountingWorkloadServer(t)
	srv.setBootstrapCode(http.StatusConflict)
	a := newTestAuthenticator(t, "", srv)

	fakeNow := time.Now()
	a.now = func() time.Time { return fakeNow }

	_, _, err := a.Token(context.Background())
	if err == nil {
		t.Fatal("expected an error on 409")
	}
	if srv.bootstrapHits.Load() != 1 {
		t.Fatalf("bootstrap hits = %d, want 1", srv.bootstrapHits.Load())
	}

	// Still inside the 60s window: no second bootstrap call.
	fakeNow = fakeNow.Add(30 * time.Second)
	_, _, err = a.Token(context.Background())
	if err == nil {
		t.Fatal("expected the negative cache to still be live")
	}
	if srv.bootstrapHits.Load() != 1 {
		t.Fatalf("bootstrap hits = %d after 30s, want still 1", srv.bootstrapHits.Load())
	}
}

func TestNegativeEntryIsNeverAToken(t *testing.T) {
	srv := newCountingWorkloadServer(t)
	srv.setBootstrapCode(http.StatusUnauthorized)
	a := newTestAuthenticator(t, "", srv)

	tok, fromCache, err := a.Token(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if tok != "" {
		t.Fatalf("expected an empty token, got %q", tok)
	}
	if fromCache {
		t.Fatal("a negative entry must never report fromCache=true")
	}
}

func TestNegativeCacheSurvivesAcrossProcesses(t *testing.T) {
	srv := newCountingWorkloadServer(t)
	srv.setBootstrapCode(http.StatusNotFound)
	path := filepath.Join(t.TempDir(), "token.json")

	fakeNow := time.Now()

	// First "process": one Authenticator instance, one cold bootstrap that
	// negatively caches to disk.
	a1 := newTestAuthenticator(t, path, srv)
	a1.now = func() time.Time { return fakeNow }
	if _, _, err := a1.Token(context.Background()); err == nil {
		t.Fatal("expected an error on 404")
	}
	if got := srv.bootstrapHits.Load(); got != 1 {
		t.Fatalf("bootstrap hits after first process = %d, want 1", got)
	}

	// Second "process": a fresh Authenticator on the same cache path, which
	// is what a second hook invocation is. Inside 60s it must not
	// bootstrap.
	a2 := newTestAuthenticator(t, path, srv)
	a2.now = func() time.Time { return fakeNow.Add(10 * time.Second) }
	if _, _, err := a2.Token(context.Background()); err == nil {
		t.Fatal("expected the negative cache to still apply")
	}
	if got := srv.bootstrapHits.Load(); got != 1 {
		t.Fatalf("bootstrap hits inside 60s = %d, want still 1", got)
	}

	// Past 60s, a third "process" bootstraps again.
	a3 := newTestAuthenticator(t, path, srv)
	a3.now = func() time.Time { return fakeNow.Add(61 * time.Second) }
	srv.setBootstrapCode(http.StatusOK)
	if _, _, err := a3.Token(context.Background()); err != nil {
		t.Fatalf("Token after the negative window: %v", err)
	}
	if got := srv.bootstrapHits.Load(); got != 2 {
		t.Fatalf("bootstrap hits after 61s = %d, want 2", got)
	}
}

func TestForeignKeyMissForcesReExchange(t *testing.T) {
	srv := newCountingWorkloadServer(t)
	path := filepath.Join(t.TempDir(), "token.json")
	a1 := newTestAuthenticator(t, path, srv)
	if _, _, err := a1.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}

	// A second Authenticator with a DIFFERENT key sharing the same cache
	// file must not reuse the first key's token.
	key2, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	encoded2, err := EncodePrivateKey(key2)
	if err != nil {
		t.Fatalf("EncodePrivateKey: %v", err)
	}
	a2, err := NewAuthenticator("obx_testkey", encoded2, path, srv.srv.URL, srv.srv.Client())
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	if _, _, err := a2.Token(context.Background()); err != nil {
		t.Fatalf("Token (foreign key): %v", err)
	}
	if got := srv.bootstrapHits.Load(); got != 2 {
		t.Fatalf("bootstrap hits = %d, want 2 (one per distinct key)", got)
	}
}

func TestErrorsNeverContainTokenAssertionOrKey(t *testing.T) {
	key := testRSAKey(t)
	doc := testDocForAssertion()
	assertion, err := BuildAssertion(key, doc)
	if err != nil {
		t.Fatalf("BuildAssertion: %v", err)
	}
	encodedKey, err := EncodePrivateKey(key)
	if err != nil {
		t.Fatalf("EncodePrivateKey: %v", err)
	}

	srv := newCountingWorkloadServer(t)
	srv.setBootstrapCode(http.StatusUnauthorized)
	a := newTestAuthenticator(t, "", srv)

	_, _, err = a.Token(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if strings.Contains(msg, assertion) {
		t.Fatal("error message contains the assertion")
	}
	if strings.Contains(msg, encodedKey) {
		t.Fatal("error message contains the encoded private key")
	}
	if strings.Contains(msg, "atk-") {
		t.Fatal("error message contains an access token")
	}
}
