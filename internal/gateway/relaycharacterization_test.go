package gateway

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
)

// The relay's header handling, its stream loop and its emit ordering have no
// covering tests. Phase 06 proposes replacing all three with
// httputil.ReverseProxy, and swapping untested hand-rolled code for stdlib is a
// change nobody could review -- there would be nothing to compare against.
//
// These are that comparison, written against the hand-rolled relay first. Each
// describes a property the gateway owes its callers rather than an
// implementation detail, so whichever relay ships has to pass them.

// relayFixture stands an upstream and a capturing gateway in front of it.
func relayFixture(t *testing.T, upstream http.HandlerFunc) (*recordingEmitter, *memhttptest.Server) {
	t.Helper()
	up := memhttptest.NewServer(t, upstream)
	t.Cleanup(up.Close)
	em := &recordingEmitter{}
	return em, serveGateway(t, wire(t, up.URL, em, nil, nil))
}

// rawExchange writes a request the net/http client refuses to build (a client
// may not set Connection freely) and reads the answer.
func rawExchange(t *testing.T, front *memhttptest.Server, request string) *http.Response {
	t.Helper()
	return rawExchangeTo(t, strings.TrimPrefix(front.URL, "http://"), request)
}

// rawExchangeTo is the same against a bare address, so the spike can reuse it.
func rawExchangeTo(t *testing.T, addr, request string) *http.Response {
	t.Helper()
	conn, err := dialFront(t, addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	if _, err := io.WriteString(conn, strings.ReplaceAll(request, "{{host}}", addr)); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp
}

// dialFront reaches a memhttptest server over its in-memory pipe, so a test can
// write bytes net/http's client refuses to send.
func dialFront(t *testing.T, addr string) (net.Conn, error) {
	t.Helper()
	conn, err := memhttptest.DialContext(context.Background(), "tcp", addr)
	if err == nil {
		t.Cleanup(func() { _ = conn.Close() })
	}
	return conn, err
}

// TestRelayStripsHopByHopHeaders. A hop-by-hop header describes this
// connection, not the message, so forwarding one corrupts the next hop.
func TestRelayStripsHopByHopHeaders(t *testing.T) {
	var seen http.Header
	_, front := relayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		io.WriteString(w, `{"ok":true}`)
	})

	resp := rawExchange(t, front, "POST /v1/messages HTTP/1.1\r\n"+
		"Host: {{host}}\r\n"+
		"Content-Length: 2\r\n"+
		"Keep-Alive: timeout=5\r\n"+
		"Proxy-Authorization: Basic c2VjcmV0\r\n"+
		"Proxy-Connection: keep-alive\r\n"+
		"X-Business-Header: kept\r\n\r\n{}")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	for _, h := range []string{"Keep-Alive", "Proxy-Authorization", "Proxy-Connection", "Upgrade"} {
		if v := seen.Get(h); v != "" {
			t.Errorf("hop-by-hop header %s reached the upstream as %q", h, v)
		}
	}
	if seen.Get("X-Business-Header") != "kept" {
		t.Errorf("an end-to-end header was dropped: X-Business-Header = %q", seen.Get("X-Business-Header"))
	}
}

// TestRelayStripsHeadersTheConnectionHeaderNames. RFC 9110: Connection lists
// header names that apply to this hop only. Most hand-rolled relays miss this
// entirely; this one gets it right, and a replacement has to as well.
func TestRelayStripsHeadersTheConnectionHeaderNames(t *testing.T) {
	var seen http.Header
	_, front := relayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		io.WriteString(w, `{"ok":true}`)
	})

	resp := rawExchange(t, front, "POST /v1/messages HTTP/1.1\r\n"+
		"Host: {{host}}\r\n"+
		"Content-Length: 2\r\n"+
		"Connection: X-Per-Hop\r\n"+
		"X-Per-Hop: must-not-forward\r\n"+
		"X-Business-Header: kept\r\n\r\n{}")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if v := seen.Get("X-Per-Hop"); v != "" {
		t.Errorf("a header named by Connection reached the upstream as %q; it applies to this hop only", v)
	}
	if seen.Get("X-Business-Header") != "kept" {
		t.Error("stripping the Connection-named header took an unrelated header with it")
	}
}

// TestRelayStreamsPerChunkRatherThanBuffering. Claude Code counts relayed bytes
// against a watchdog and SSE keepalives hold a long thinking pause open, so a
// buffering relay stalls real sessions.
func TestRelayStreamsPerChunkRatherThanBuffering(t *testing.T) {
	release := make(chan struct{})
	handlerDone := make(chan struct{})
	_, front := relayFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		defer close(handlerDone)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: first\n\n")
		w.(http.Flusher).Flush()
		<-release // the first frame must already have reached the client
		io.WriteString(w, "event: second\n\n")
	})

	resp, err := probeClient().Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("relayed request: %v", err)
	}
	defer resp.Body.Close()

	const frame = "event: first\n\n"
	first := make([]byte, len(frame))
	read := make(chan error, 1)
	go func() { _, err := io.ReadFull(resp.Body, first); read <- err }()
	select {
	case err := <-read:
		if err != nil {
			t.Fatalf("reading the first frame: %v", err)
		}
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("the first frame never arrived while the upstream was still streaming: the relay buffered")
	}
	if string(first) != frame {
		t.Errorf("first frame = %q, want %q", first, frame)
	}
	close(release)
	<-handlerDone
	io.Copy(io.Discard, resp.Body)
}

// TestRelayEmitsExactlyOnceOnASuccessfulCall.
func TestRelayEmitsExactlyOnceOnASuccessfulCall(t *testing.T) {
	em, front := relayFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
	})

	resp, err := probeClient().Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{"m":1}`))
	if err != nil {
		t.Fatalf("relayed request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got := em.await(t, 1)
	if len(got) != 1 {
		t.Fatalf("emitted %d records for one call, want exactly 1", len(got))
	}
	if got[0].HTTPStatus != http.StatusOK {
		t.Errorf("emitted http_status = %d, want 200", got[0].HTTPStatus)
	}
	if !strings.Contains(got[0].ResponseBody, `"ok":true`) {
		t.Errorf("emitted response body = %q, want the upstream's", got[0].ResponseBody)
	}
	if !strings.Contains(got[0].RequestBody, `"m":1`) {
		t.Errorf("emitted request body = %q, want the client's", got[0].RequestBody)
	}
}

// TestRelayEmitsEvidenceWhenTheUpstreamIsUnreachable. A call that happened and
// was not recorded is indistinguishable from one that never happened, so the
// failure has to leave evidence too.
func TestRelayEmitsEvidenceWhenTheUpstreamIsUnreachable(t *testing.T) {
	em := &recordingEmitter{}
	front := serveGateway(t, wire(t, "http://127.0.0.1:1", em, nil, nil))

	resp, err := probeClient().Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("relayed request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}

	got := em.await(t, 1)
	if got[0].HTTPStatus != 0 {
		t.Errorf("emitted http_status = %d, want 0 for a call that never reached the upstream", got[0].HTTPStatus)
	}
}

// TestRelayRefusalShapeIsFixed. The refusal body is this package's own wording
// and a client parses it, so a replacement must keep it byte-identical.
func TestRelayRefusalShapeIsFixed(t *testing.T) {
	em, front := relayFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("the upstream was reached on a request the relay should have refused")
	})

	resp := rawExchange(t, front,
		"GET http://elsewhere.test/v1 HTTP/1.1\r\nHost: {{host}}\r\n\r\n")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	const want = `{"type":"error","error":{"type":"openbox_gateway_error","message":"request target must be origin-form (a path beginning with /)"}}`
	if string(body) != want {
		t.Errorf("refusal body:\n got %s\nwant %s", body, want)
	}
	if n := len(em.all()); n != 0 {
		t.Errorf("emitted %d records for a request that was never relayed", n)
	}
}

// TestRelayAddsNoForwardingHeadersOfItsOwn. httputil.ReverseProxy's Director
// API appends X-Forwarded-For by default. Forwarding here is byte-identical,
// and telling the provider about the developer's machine is a privacy change
// nobody asked for.
func TestRelayAddsNoForwardingHeadersOfItsOwn(t *testing.T) {
	var seen http.Header
	_, front := relayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		io.WriteString(w, `{"ok":true}`)
	})

	resp, err := probeClient().Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("relayed request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"} {
		if v := seen.Get(h); v != "" {
			t.Errorf("the relay added %s: %q", h, v)
		}
	}
}

// TestRelayCopiesTheResponseStatusAndHeaders, minus the hop-by-hop set.
func TestRelayCopiesTheResponseStatusAndHeaders(t *testing.T) {
	_, front := relayFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "req_abc")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "42")
		w.Header().Set("Keep-Alive", "timeout=5")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"type":"error"}`)
	})

	resp, err := probeClient().Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("relayed request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429: a rate-limit answer the client must see", resp.StatusCode)
	}
	if resp.Header.Get("X-Request-Id") != "req_abc" {
		t.Errorf("X-Request-Id = %q, want the upstream's", resp.Header.Get("X-Request-Id"))
	}
	if resp.Header.Get("Anthropic-Ratelimit-Requests-Remaining") != "42" {
		t.Error("a provider rate-limit header was dropped")
	}
	if v := resp.Header.Get("Keep-Alive"); v != "" {
		t.Errorf("a hop-by-hop response header reached the client as %q", v)
	}
}

// TestRelayCaptureIsBoundedOnALongStream. A long SSE response must not hold the
// whole conversation in process memory, and the client must still get all of it.
func TestRelayCaptureIsBoundedOnALongStream(t *testing.T) {
	const chunks = 400
	chunk := strings.Repeat("d", 4<<10) // 1.6 MiB, past the sink's cap
	em, front := relayFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for range chunks {
			io.WriteString(w, chunk)
			w.(http.Flusher).Flush()
		}
	})

	resp, err := probeClient().Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("relayed request: %v", err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("draining the stream: %v", err)
	}
	if n != int64(chunks*len(chunk)) {
		t.Errorf("client received %d bytes, want %d: the relay dropped part of the stream", n, chunks*len(chunk))
	}

	got := em.await(t, 1)
	if len(got[0].ResponseBody) >= chunks*len(chunk) {
		t.Errorf("captured %d bytes of a %d-byte stream; the sink is not bounded",
			len(got[0].ResponseBody), chunks*len(chunk))
	}
	if len(got[0].ResponseBody) == 0 {
		t.Error("captured nothing at all, so the bound assertion above proves nothing")
	}
}
