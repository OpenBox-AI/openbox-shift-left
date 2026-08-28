package gateway

import (
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"

	"github.com/openbox-ai/openbox-shift-left/client/memhttptest"
	"strings"
	"testing"
	"time"
)

// relayfailure_test.go covers the paths where the relay does NOT complete
// normally. Every one of them used to be silent in the direction that matters:
// either the client was told a broken exchange had finished cleanly, or the
// governance record of a real call was simply absent.

// TestUnreachableUpstreamStillProducesEvidence is the assurance property.
//
// The transport writes the request body upstream before Do returns, so by the
// time this fails the prompt has already left the machine. Returning without an
// emit made that call invisible — and because a caller controls when it hangs up,
// it was a supported way for any local process to relay a prompt and suppress its
// own record. ADR-0021 §2 rests on a bypass leaving a HOLE in the record; this
// left nothing at all.
func TestUnreachableUpstreamStillProducesEvidence(t *testing.T) {
	memhttptest.RequireBind(t)
	// A closed port: Do fails without any server involved.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close()

	em := &recordingEmitter{}
	srv := serveGateway(t, wire(t, dead, em, nil, nil))

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{"prompt":"unrecorded"}`))
	req.Header.Set("Authorization", fixtureCredential)
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want 502", resp.StatusCode)
	}

	got := em.await(t, 1)[0]
	if got.HTTPMethod != http.MethodPost || !strings.HasSuffix(got.HTTPURL, "/v1/messages") {
		t.Errorf("evidence does not identify the call: method=%q url=%q", got.HTTPMethod, got.HTTPURL)
	}
	// Status 0 is the point: a fabricated code would be indistinguishable from
	// one the provider sent.
	if got.HTTPStatus != 0 {
		t.Errorf("HTTPStatus = %d, want 0 — no response was observed, so none may be claimed", got.HTTPStatus)
	}
	if got.CredentialFingerprint == "" {
		t.Error("no credential fingerprint — the call cannot be attributed")
	}
}

// TestCancelledRelayStillProducesEvidence is the same property via the path a
// developer actually takes: pressing Esc mid-turn.
func TestCancelledRelayStillProducesEvidence(t *testing.T) {
	// An upstream that accepts the request and then stalls, so the cancel lands
	// after the body was sent and before a response exists.
	//
	// The stall is BOUNDED. Waiting only on r.Context().Done() deadlocks the
	// teardown: Close waits for this handler, this handler waits for the gateway's
	// outbound connection to drop, and that drop is what Close is waiting to
	// observe. A ceiling breaks the cycle without weakening the case — the cancel
	// still arrives first.
	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(600 * time.Millisecond):
		}
	}))
	t.Cleanup(upstream.Close)

	em := &recordingEmitter{}
	srv := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/messages",
		strings.NewReader(`{"prompt":"unrecorded"}`))
	req.Header.Set("Authorization", fixtureCredential)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := probeClient().Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	got := em.await(t, 1)[0]
	if got.HTTPStatus != 0 {
		t.Errorf("HTTPStatus = %d, want 0", got.HTTPStatus)
	}
	if got.CredentialFingerprint == "" {
		t.Error("a cancelled call left no attributable record")
	}
}

// TestBrokenStreamIsNotRelayedAsACleanEnd is the truncation control.
//
// An upstream that dies mid-body must not reach the client as a well-formed
// end-of-stream. It did: the handler returned normally, Go wrote the terminating
// chunk, and Claude Code saw a truncated assistant message that looked finished —
// so it neither retried nor errored. httputil.ReverseProxy panics with
// ErrAbortHandler for exactly this (Go issue 23643).
func TestBrokenStreamIsNotRelayedAsACleanEnd(t *testing.T) {
	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Declare more than we send, then hang up: the client's read of the body
		// fails rather than ending.
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\n\n"))
		http.NewResponseController(w).Flush()
		panic(http.ErrAbortHandler) // kill the connection mid-body
	}))
	t.Cleanup(upstream.Close)

	em := &recordingEmitter{}
	srv := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

	resp, err := probeClient().Get(srv.URL + "/v1/messages")
	if err != nil {
		// A connection-level failure is an acceptable outcome too: the point is
		// that the client is NOT told the stream ended cleanly.
		return
	}
	defer resp.Body.Close()
	_, readErr := io.ReadAll(resp.Body)
	if readErr == nil {
		t.Error("the client read a clean end-of-stream from a stream that broke upstream")
	}

	// The failed call is still evidence — and it is emitted before the abort.
	if got := em.await(t, 1); got[0].HTTPStatus != http.StatusOK {
		t.Errorf("HTTPStatus = %d, want the observed 200", got[0].HTTPStatus)
	}
}

// TestContentEncodedBodyIsNotFedToTheRedactor is the redaction guarantee.
//
// DisableCompression stops the TRANSPORT asking for gzip, but the client's own
// Accept-Encoding is relayed verbatim (TestClientAcceptEncodingSurvives asserts
// that on purpose), so an upstream can legitimately answer compressed. Those
// bytes went straight into the shared secret detector, which matched nothing: a
// response carrying a credential was captured with no redaction marker, was not
// valid UTF-8, and json.Marshal then rewrote every byte to U+FFFD. The capture
// must decline instead of storing binary it cannot inspect.
func TestContentEncodedBodyIsNotFedToTheRedactor(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLE"
	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		zw := gzip.NewWriter(w)
		_, _ = zw.Write([]byte(`{"text":"here is the key ` + secret + `"}`))
		_ = zw.Close()
	}))
	t.Cleanup(upstream.Close)

	em := &recordingEmitter{}
	srv := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

	resp, err := probeClient().Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()

	body := em.await(t, 1)[0].ResponseBody
	if strings.Contains(body, secret) {
		t.Errorf("the credential was captured unredacted out of a compressed body: %q", body)
	}
	if !strings.Contains(body, "content-encoded") {
		t.Errorf("a compressed body was captured as bytes rather than declined: %q", body)
	}
}

// TestCapturedURLKeepsPercentEscapes pins the recorded destination to the path
// that was actually forwarded.
//
// r.URL.Path is DECODED, so stripQuery cut the record at a %3F and a %2F became a
// path separator: the stored http.url was not where the call went. ServeHTTP's
// origin-form guard exists because a misrecorded destination leaves no hole in
// the record; the host was defended and the path was not.
func TestCapturedURLKeepsPercentEscapes(t *testing.T) {
	for _, target := range []string{"/v1/mess%3Fages", "/v1/a%2Fb", "/v1/plain"} {
		t.Run(target, func(t *testing.T) {
			var got recorded
			upstream := upstreamRecorder(t, &got, nil)
			em := &recordingEmitter{}
			srv := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

			// A raw request so the escape survives the client.
			req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.URL.Opaque = target
			resp, err := probeClient().Do(req)
			if err != nil {
				t.Fatalf("request through gateway: %v", err)
			}
			resp.Body.Close()

			if got.target != target {
				t.Fatalf("upstream saw %q, want the verbatim %q", got.target, target)
			}
			if url := em.await(t, 1)[0].HTTPURL; !strings.HasSuffix(url, target) {
				t.Errorf("recorded http.url %q does not end in the forwarded target %q", url, target)
			}
		})
	}
}

// TestFragmentInRequestTargetIsRefused: a '#' cannot be relayed byte-identically
// (url.Parse splits it off and Request.write drops it), so it is refused rather
// than silently truncated.
func TestFragmentInRequestTargetIsRefused(t *testing.T) {
	var got recorded
	upstream := upstreamRecorder(t, &got, nil)
	srv := serveGateway(t, wire(t, upstream.URL, nil, nil, nil))

	conn, err := memhttptest.DialContext(context.Background(), "tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET /v1/messages#frag HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(conn)
	if !strings.Contains(string(raw), "400") {
		t.Errorf("a fragment in the request target was not refused: %q", firstLine(string(raw)))
	}
	if got.target != "" {
		t.Errorf("the upstream was reached with %q — the fragment was silently dropped instead of refused", got.target)
	}
}

// TestProxyConnectionIsNotForwarded: it is hop-by-hop, and it is the one such
// header that appears INSTEAD of being named in a Connection value, so
// connectionNamedHeaders cannot cover it.
func TestProxyConnectionIsNotForwarded(t *testing.T) {
	var got recorded
	upstream := upstreamRecorder(t, &got, nil)
	srv := serveGateway(t, wire(t, upstream.URL, nil, nil, nil))

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Proxy-Connection", "keep-alive")
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()
	if v := got.header.Get("Proxy-Connection"); v != "" {
		t.Errorf("Proxy-Connection reached the provider as %q", v)
	}
}

// TestResponseHeadersReachTheClientBeforeTheFirstBodyByte.
//
// Go buffers WriteHeader, and the only flush was inside `if n > 0` — so an
// upstream that sends headers and then pauses relayed as total silence. This
// package's whole no-buffering rationale is Claude Code's byte watchdog, and
// response headers are bytes.
func TestResponseHeadersReachTheClientBeforeTheFirstBodyByte(t *testing.T) {
	const pause = 400 * time.Millisecond
	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush()
		time.Sleep(pause)
		_, _ = w.Write([]byte(": ping\n\n"))
	}))
	t.Cleanup(upstream.Close)

	srv := serveGateway(t, wire(t, upstream.URL, nil, nil, nil))

	start := time.Now()
	resp, err := probeClient().Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	defer resp.Body.Close()
	// Do returns once the response HEADERS are in.
	if waited := time.Since(start); waited >= pause {
		t.Errorf("response headers took %v, i.e. they waited for the first body byte (%v pause)", waited, pause)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
