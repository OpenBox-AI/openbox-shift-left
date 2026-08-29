package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/gatewayemit"
	"github.com/openbox-ai/openbox-shift-left/client/memhttptest"
	"github.com/openbox-ai/openbox-shift-left/gateway/gatewaytest"
	"github.com/openbox-ai/openbox-shift-left/transport"
)

// transportreplay_test.go — a REAL recorded model call crossing the CONNECT path.
//
// Until this existed, no response body had ever traversed this lane. The control
// test next door proves the whole chain records, but its upstream is a refused
// loopback port, so everything it measures happens on the request side; the
// spike suite that measured byte-identity ran the PLAIN-HTTP path, which is
// different code from goproxy's hijack. Phase 11's report and the spike's own
// report both named running the CONNECT path as the first thing conformance
// should do. This is that.
//
// One substitution, and it is the smallest one available: the relay's upstream
// DIAL is pointed at an in-memory server (gatewaytest.SwapUpstreamDial). The
// relay's own Transport — DisableCompression, ForceAttemptHTTP2, the redirect
// rule, the idle pool — stays in the path, and the Accept-Encoding assertion
// below is what proves it did: if the Transport had been substituted wholesale,
// Go would advertise gzip on the client's behalf and the header arriving upstream
// would not be the client's own.

type recordedExchange struct {
	Request struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"request"`
	Response struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"response"`
}

func loadExchange(t *testing.T, name string) recordedExchange {
	t.Helper()
	path := filepath.Join("..", "..", "..", "transport", "testdata", "corpus", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recorded exchange %s: %v", path, err)
	}
	var ex recordedExchange
	if err := json.Unmarshal(raw, &ex); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if ex.Request.Body == "" || ex.Response.Body == "" {
		t.Fatalf("%s carries an empty body on one side; the identity assertions would be vacuous", name)
	}
	return ex
}

// hopByHop are the headers a relay owns rather than forwards. They are excluded
// from the identity assertion because forwarding them would be the BUG.
var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-connection": true,
	"transfer-encoding": true, "upgrade": true, "content-length": true,
	"host": true,
}

// connectAndHandshake performs the client half of an intercepted CONNECT.
//
// Every step is ordered the way a real client orders it, and the reason is
// recorded where it bites: on a pipe a write BLOCKS until it is read, so reading
// the CONNECT response before starting the handshake is not politeness — skipping
// it deadlocks, and a deadlocked go test reads as an environment problem rather
// than as an answer.
func connectAndHandshake(t *testing.T, p *transport.Proxy, ca *transport.CA, host string) *tls.Conn {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close(); serverConn.Close() })
	go p.ServeIntercepted(serverConn, host+":443")

	if err := clientConn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	br := bufio.NewReader(clientConn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read CONNECT status: %v", err)
	}
	if !strings.HasPrefix(status, "HTTP/1.1 200") {
		t.Fatalf("CONNECT status = %q, want 200", status)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read CONNECT headers: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	if br.Buffered() != 0 {
		t.Fatalf("%d bytes buffered past the CONNECT response; the TLS handshake would lose them", br.Buffered())
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("CA PEM did not parse")
	}
	tc := tls.Client(clientConn, &tls.Config{ServerName: host, RootCAs: pool})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("TLS handshake against the project CA: %v", err)
	}
	return tc
}

// TestTransportRelaysARecordedExchangeByteIdentically is the CONNECT-path
// identity case.
func TestTransportRelaysARecordedExchangeByteIdentically(t *testing.T) {
	ex := loadExchange(t, "messages-json.json")
	const sessionID = "7f3c9b2e-0000-5000-a000-00000000beef"

	var (
		gotReq  *http.Request
		gotBody []byte
	)
	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = r.Clone(r.Context())
		gotBody, _ = io.ReadAll(r.Body)
		for k, v := range ex.Response.Headers {
			if hopByHop[strings.ToLower(k)] || strings.EqualFold(k, "content-encoding") {
				continue
			}
			w.Header().Set(k, v)
		}
		w.WriteHeader(ex.Response.Status)
		_, _ = io.WriteString(w, ex.Response.Body)
	}))
	t.Cleanup(upstream.Close)

	// Only the DIAL is replaced. See the package doc on gatewaytest for exactly
	// what that buys and what it does not.
	gatewaytest.SwapUpstreamDial(t, memhttptest.DialContext)

	spoolDir := t.TempDir()
	warn := &warnLog{}
	ca, err := transport.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	em := &gatewayemit.Emitter{
		Lane:  gatewayemit.LaneProxy,
		Spool: hookflow.Spool{Dir: spoolDir},
		DID:   func() string { return "did:aip:7f3c9b2e-0000-5000-a000-00000000feed" },
		Warn:  warn.record,
	}
	p, err := transport.New(transport.Config{Upstream: upstream.URL}, ca, em)
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}

	tc := connectAndHandshake(t, p, ca, "api.anthropic.com")

	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages",
		strings.NewReader(ex.Request.Body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	sent := map[string]string{}
	for k, v := range ex.Request.Headers {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		req.Header.Set(k, v)
		sent[http.CanonicalHeaderKey(k)] = v
	}
	req.Header.Set("X-Claude-Code-Session-Id", sessionID)
	sent["X-Claude-Code-Session-Id"] = sessionID
	if err := req.Write(tc); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}

	if gotReq == nil {
		t.Fatal("the request never reached the upstream; nothing below measures anything")
	}

	// Request direction.
	if gotReq.Method != http.MethodPost || gotReq.URL.Path != "/v1/messages" {
		t.Errorf("upstream saw %s %s, want POST /v1/messages", gotReq.Method, gotReq.URL.Path)
	}
	if string(gotBody) != ex.Request.Body {
		t.Errorf("request body was modified in flight: %d bytes arrived, %d were sent",
			len(gotBody), len(ex.Request.Body))
	}
	for k, want := range sent {
		if got := gotReq.Header.Get(k); got != want {
			t.Errorf("request header %s arrived as %q, sent %q", k, got, want)
		}
	}
	// The two mutations a proxy adds by default, and the reason this is not
	// paranoia: the identity suite was first run against
	// httputil.NewSingleHostReverseProxy and failed on exactly these.
	if v := gotReq.Header.Get("X-Forwarded-For"); v != "" {
		t.Errorf("the relay injected X-Forwarded-For: %q", v)
	}
	if got, want := gotReq.Header.Get("Accept-Encoding"), sent["Accept-Encoding"]; want != "" && got != want {
		t.Errorf("Accept-Encoding arrived as %q, client sent %q — the relay's own Transport settings "+
			"(DisableCompression) are no longer in the path", got, want)
	}

	// Response direction.
	if resp.StatusCode != ex.Response.Status {
		t.Errorf("status = %d, recorded %d", resp.StatusCode, ex.Response.Status)
	}
	if string(body) != ex.Response.Body {
		t.Errorf("response body was modified in flight: %d bytes received, %d recorded",
			len(body), len(ex.Response.Body))
	}

	// And the evidence: a response body finally traversed this lane, so the
	// capture has something a request-only run could never show.
	ev := readOneSpooledEvent(t, spoolDir, waitForSpool(t, spoolDir, sessionID, warn))
	if ev.Span == nil {
		t.Fatal("no span on the spooled event")
	}
	if ev.Span.HTTPStatus != ex.Response.Status {
		t.Errorf("recorded http_status = %d, relayed %d", ev.Span.HTTPStatus, ex.Response.Status)
	}
	if ev.ProxyRequestID == "" {
		t.Error("no proxy_request_id, so the event's activity_id is empty")
	}
}

// TestTransportStreamsARecordedSSEResponsePerChunk is the streaming half, on the
// CONNECT path.
//
// It is written so that the failure it hunts — the relay buffering the response —
// is a BOUNDED failure rather than a hang. A buffering relay cannot deliver the
// first frame until the handler returns, and the handler does not return until
// the client has read the first frame, so a buffered relay deadlocks. The
// deadlock is caught by a timeout that says what it means; without the bound this
// test would stall and read as a broken environment.
func TestTransportStreamsARecordedSSEResponsePerChunk(t *testing.T) {
	ex := loadExchange(t, "messages-sse.json")
	frames := strings.SplitAfter(ex.Response.Body, "\n\n")
	if len(frames) < 3 {
		t.Fatalf("the recorded SSE body has %d frame(s); per-chunk delivery needs at least two", len(frames))
	}
	first, rest := frames[0], strings.Join(frames[1:], "")

	readFirst := make(chan struct{})
	handlerDone := make(chan struct{})
	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		select {
		case <-readFirst:
		case <-time.After(20 * time.Second):
			// Falling through writes the rest anyway, so the client unblocks and
			// the assertion below reports "buffered" instead of the whole test
			// timing out with no explanation.
		}
		_, _ = io.WriteString(w, rest)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)

	gatewaytest.SwapUpstreamDial(t, memhttptest.DialContext)

	ca, err := transport.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	em := &gatewayemit.Emitter{
		Lane:  gatewayemit.LaneProxy,
		Spool: hookflow.Spool{Dir: t.TempDir()},
		DID:   func() string { return "did:aip:7f3c9b2e-0000-5000-a000-00000000feed" },
		Warn:  func(string, ...any) {},
	}
	p, err := transport.New(transport.Config{Upstream: upstream.URL}, ca, em)
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}

	tc := connectAndHandshake(t, p, ca, "api.anthropic.com")

	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages",
		strings.NewReader(ex.Request.Body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claude-Code-Session-Id", "7f3c9b2e-0000-5000-a000-0000000000ff")
	if err := req.Write(tc); err != nil {
		t.Fatalf("write request: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(tc), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, len(first))
	streamed := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(resp.Body, buf)
		streamed <- err
	}()
	select {
	case err := <-streamed:
		if err != nil {
			t.Fatalf("reading the first SSE frame: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first SSE frame never arrived while the upstream was still streaming: " +
			"the relay BUFFERED the response. Claude Code counts relayed bytes against a 180s " +
			"watchdog and SSE keepalives are what hold a long thinking pause open, so a buffering " +
			"relay stalls real sessions.")
	}
	if string(buf) != first {
		t.Errorf("first frame arrived modified:\n got %q\nwant %q", string(buf), first)
	}
	close(readFirst)

	tail, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the rest of the stream: %v", err)
	}
	if string(buf)+string(tail) != ex.Response.Body {
		t.Errorf("the streamed body differs from the recorded one: %d bytes received, %d recorded",
			len(buf)+len(tail), len(ex.Response.Body))
	}
	<-handlerDone
}

// warnLog collects the emitter's warnings so a failure can quote them.
//
// The emitter must NOT be handed t.Logf: the capture completes on the relay's own
// goroutine, and a log after the test function returns panics the whole binary —
// which would read as a crash in the code under test rather than as a race in the
// harness.
type warnLog struct {
	mu    sync.Mutex
	lines []string
}

func (w *warnLog) record(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (w *warnLog) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.lines) == 0 {
		return "(the emitter reported nothing)"
	}
	return strings.Join(w.lines, "; ")
}

// waitForSpool blocks until the lane's spool file exists, bounded.
//
// The capture completes on the relay's goroutine AFTER the client has read the
// last byte, so reading the spool the instant the response ends is a race the
// test loses about as often as it wins. Bounded rather than a bare poll loop: an
// unbounded wait turns "the capture never happened" into a hung `go test`, which
// reads as a broken environment instead of as the answer.
func waitForSpool(t *testing.T, spoolDir, sessionID string, warn *warnLog) string {
	t.Helper()
	path := hookflow.Spool{Dir: spoolDir}.SessionPath(sessionID)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return sessionID
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing reached the spool for session %s within 10s. The relay answered, so the break "+
		"is between the capture and the spool. Emitter said: %s", sessionID, warn)
	return sessionID
}

// TestTransportAddsNoCompressionHeaderOfItsOwn is the control for
// DisableCompression, and it exists because the obvious version of that
// assertion does not work.
//
// Asserting that a client's OWN Accept-Encoding survives cannot detect
// DisableCompression being switched off: net/http only adds the header when the
// request does not already carry one, so with the fixture's header present both
// settings behave identically and the mutation drill stays green — measured, not
// assumed. The state that separates them is a request with NO Accept-Encoding,
// where a compressing transport advertises gzip on the developer's behalf,
// changing the bytes the provider sees and silently decompressing the response
// the relay is supposed to forward untouched.
func TestTransportAddsNoCompressionHeaderOfItsOwn(t *testing.T) {
	var got http.Header
	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)

	gatewaytest.SwapUpstreamDial(t, memhttptest.DialContext)

	ca, err := transport.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	em := &gatewayemit.Emitter{
		Lane:  gatewayemit.LaneProxy,
		Spool: hookflow.Spool{Dir: t.TempDir()},
		DID:   func() string { return "did:aip:7f3c9b2e-0000-5000-a000-00000000feed" },
		Warn:  func(string, ...any) {},
	}
	p, err := transport.New(transport.Config{Upstream: upstream.URL}, ca, em)
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}

	tc := connectAndHandshake(t, p, ca, "api.anthropic.com")

	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages",
		strings.NewReader(`{"model":"claude-opus-5","messages":[]}`))
	if err != nil {
		t.Fatalf("compose request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claude-Code-Session-Id", "7f3c9b2e-0000-5000-a000-0000000000ae")
	// Deliberately no Accept-Encoding.
	if err := req.Write(tc); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if got == nil {
		t.Fatal("the request never reached the upstream; the assertion below would be vacuous")
	}
	if v := got.Get("Accept-Encoding"); v != "" {
		t.Errorf("the relay advertised Accept-Encoding: %q on a request that carried none — "+
			"DisableCompression is no longer in the path, so the provider sees different bytes "+
			"than the client sent and net/http will silently decompress the response the relay "+
			"is meant to forward untouched", v)
	}
}
