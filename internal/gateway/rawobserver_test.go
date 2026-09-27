package gateway

import (
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// secretShapedValue is a keyword-adjacent, secret-shaped literal built from a
// base64 blob at runtime rather than written as a plain literal: a governing
// local hook rewrites a matching literal in an edited/written file into a
// placeholder on disk, which would make this very test assert against the
// wrong (already-redacted) fixture. See credentials-and-secrets.md's note on
// deriving such fixtures in code.
func secretShapedValue() string {
	b, err := base64.StdEncoding.DecodeString("cHdkOmh1bnRlcjItbm90LWEtcmVhbC1zZWNyZXQ=")
	if err != nil {
		panic(err)
	}
	return string(b)
}

// rawRecorder collects every RawCapture WithRawObserver fires, mirroring
// recordingEmitter's shape for the redacted side.
type rawRecorder struct {
	mu   sync.Mutex
	seen []RawCapture
}

func (r *rawRecorder) observe(c RawCapture) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, c)
}

func (r *rawRecorder) all() []RawCapture {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RawCapture(nil), r.seen...)
}

func (r *rawRecorder) await(t *testing.T, n int) []RawCapture {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := r.all(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := r.all()
	t.Fatalf("observed %d raw captures, want %d", len(got), n)
	return got
}

// TestRawObserverSeesUnredactedBodyWhileCapturedStaysRedacted is phase 3's
// contract for the gateway seam: WithRawObserver must see the body exactly
// as it came off the wire, secret-shaped fixture and all, at the same time
// the ordinary Emitter -- wired in the very same call -- keeps seeing it
// redacted. One relayed call, two views, and the raw one must never leak
// into the redacted one or vice versa.
func TestRawObserverSeesUnredactedBodyWhileCapturedStaysRedacted(t *testing.T) {
	secret := secretShapedValue()
	secretShapedBody := `{"prompt":"` + secret + ` please help"}`
	secretShapedReply := `{"reply":"` + secret + ` ok"}`

	var got recorded
	upstream := upstreamRecorder(t, &got, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(secretShapedReply))
	})

	g, err := New(Config{Upstream: upstream.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	em := &recordingEmitter{}
	raw := &rawRecorder{}
	g = g.WithCapture(em).WithRawObserver(raw.observe)

	gw := serveGateway(t, g)
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(secretShapedBody))
	req.Header.Set("Authorization", fixtureCredential)
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	resp.Body.Close()

	captures := em.await(t, 1)
	rawCaptures := raw.await(t, 1)

	c := captures[0]
	r := rawCaptures[0]

	if strings.Contains(c.RequestBody, secret) {
		t.Errorf("the redacted Captured.RequestBody still carries the secret-shaped value: %q", c.RequestBody)
	}
	if strings.Contains(c.ResponseBody, secret) {
		t.Errorf("the redacted Captured.ResponseBody still carries the secret-shaped value: %q", c.ResponseBody)
	}
	if !strings.Contains(r.RequestBody, secret) {
		t.Errorf("the raw observer's RequestBody was redacted: %q", r.RequestBody)
	}
	if !strings.Contains(r.ResponseBody, secret) {
		t.Errorf("the raw observer's ResponseBody was redacted: %q", r.ResponseBody)
	}
	if r.RequestBody != secretShapedBody {
		t.Errorf("raw RequestBody = %q, want the exact wire body %q", r.RequestBody, secretShapedBody)
	}
	// Headers stay scrubbed even in the raw view: never the credential itself.
	if got := r.RequestHeaders["Authorization"]; got != redactedHeaderValue {
		t.Errorf("raw observer's Authorization header = %q, want it scrubbed like every other view", got)
	}
	if r.Status != http.StatusOK {
		t.Errorf("raw Status = %d, want 200", r.Status)
	}
	if r.Method != http.MethodPost {
		t.Errorf("raw Method = %q, want POST", r.Method)
	}
}

// TestRawObserverFiresWithoutAnEmitter: the raw observer must not depend on
// WithCapture being wired at all -- a lane that wants full local trace fidelity
// but has, say, no elected producer for this call must still see it raw.
func TestRawObserverFiresWithoutAnEmitter(t *testing.T) {
	var got recorded
	upstream := upstreamRecorder(t, &got, nil)

	g, err := New(Config{Upstream: upstream.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	raw := &rawRecorder{}
	g = g.WithRawObserver(raw.observe)

	gw := serveGateway(t, g)
	resp, err := probeClient().Get(gw.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	resp.Body.Close()

	raw.await(t, 1)
}
