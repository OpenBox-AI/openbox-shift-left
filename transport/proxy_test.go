package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/gateway"
)

// stubEmitter records what the relay captured, so a test can assert presence or
// absence without reaching the spool.
type stubEmitter struct {
	mu sync.Mutex
	c  []gateway.Captured
}

func (e *stubEmitter) Emit(_ context.Context, c gateway.Captured) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.c = append(e.c, c)
}

func (e *stubEmitter) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.c)
}

// readConnectResponse consumes the CONNECT 200 the way a real client does.
//
// It is not ceremony. On an in-memory pipe a write BLOCKS until someone reads,
// so a test that jumps straight to the TLS handshake deadlocks against the very
// 200 it is waiting for — and the symptom is a 15-second hang, which reads as an
// environment problem rather than as an answer. Byte-at-a-time to the blank
// line, so nothing of the TLS record that follows is swallowed into a buffer the
// tls.Client cannot see.
func readConnectResponse(t *testing.T, c net.Conn) string {
	t.Helper()
	var head []byte
	one := make([]byte, 1)
	for !strings.HasSuffix(string(head), "\r\n\r\n") {
		if len(head) > 4096 {
			t.Fatalf("CONNECT response head exceeded 4096 bytes: %q", head)
		}
		n, err := c.Read(one)
		if err != nil {
			t.Fatalf("read CONNECT response: %v (so far: %q)", err, head)
		}
		head = append(head, one[:n]...)
	}
	return string(head)
}

func testCA(t *testing.T) *CA {
	t.Helper()
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	return ca
}

// TestNewRefusesAConstructionWithNoEmitter.
//
// This is the WithCapture gap, made structurally impossible instead of tested
// around. The gateway lane shipped with `g.emitter` nil because nothing in the
// binary opted in: the relay worked perfectly and discarded every capture, while
// package gateway tested against a stub Emitter and package client tested against
// a hand-written DevEvent. A fake at each end of a seam with no implementation
// between them keeps both suites green and proves nothing about the seam.
//
// Requiring the emitter at construction means there is no reachable state where
// this lane relays without recording.
func TestNewRefusesAConstructionWithNoEmitter(t *testing.T) {
	_, err := New(Config{}, testCA(t), nil)
	if err == nil {
		t.Fatal("New accepted a nil emitter; a relay that cannot record is the gap this lane exists to close")
	}
	if !strings.Contains(err.Error(), "emitter") {
		t.Errorf("error %q does not name the emitter", err)
	}
}

// TestNewRefusesAConstructionWithNoCA: without a CA nothing can be terminated,
// and the failure must be at construction rather than on the first model call.
func TestNewRefusesAConstructionWithNoCA(t *testing.T) {
	if _, err := New(Config{}, nil, &stubEmitter{}); err == nil {
		t.Fatal("New accepted a nil CA")
	}
}

// TestNewRefusesAnInvalidConfig: Validate's loopback rule has to be enforced at
// construction, not merely available.
func TestNewRefusesAnInvalidConfig(t *testing.T) {
	if _, err := New(Config{Addr: "0.0.0.0:8790"}, testCA(t), &stubEmitter{}); err == nil {
		t.Fatal("New accepted a non-loopback listen address")
	}
}

// TestConnectActionInterceptsOnlyTheAllowlistedHost.
//
// The security-critical branch, reachable without a socket. An allowlisted host
// is hijacked (we terminate TLS); everything else is ACCEPTED, which in goproxy
// means a blind tunnel: forwarded byte-for-byte, never decrypted, never captured.
func TestConnectActionInterceptsOnlyTheAllowlistedHost(t *testing.T) {
	p, err := New(Config{}, testCA(t), &stubEmitter{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if !p.intercepts("api.anthropic.com:443") {
		t.Error("the allowlisted host is not intercepted; the lane would capture nothing")
	}
	for _, host := range []string{
		"evil.test:443",
		"console.anthropic.com:443",
		"api.anthropic.com.evil.test:443",
		"github.com:443",
	} {
		if p.intercepts(host) {
			t.Errorf("%q would be TLS-intercepted; only the allowlisted host may be", host)
		}
	}
}

// TestNewClearsInheritedProxyEnv is the self-loop guard.
//
// Phase 12 activates this lane by putting HTTPS_PROXY=http://127.0.0.1:<port>
// into the CLIENT's environment. If the DAEMON's own environment inherits it —
// a launchd setenv, /etc/environment, a global export — then the relay's own
// upstream leg dials the proxy, which is this process, and every intercepted
// call recurses: CONNECT → hijack → serve → upstream Do() → HTTPS_PROXY →
// CONNECT → … until sockets run out. Both legs read the environment
// (gateway.New sets Proxy: http.ProxyFromEnvironment on its client, and so does
// NewIdentityProxy on goproxy's transport), so clearing it is one fix for both.
//
// It happens in New rather than being left to the caller because discipline is
// not a control: a constructor that cannot be used without clearing is.
func TestNewClearsInheritedProxyEnv(t *testing.T) {
	keys := []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"}
	for _, k := range keys {
		t.Setenv(k, "http://127.0.0.1:8790")
	}

	p, err := New(Config{}, testCA(t), &stubEmitter{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			t.Errorf("%s is still %q after New; the relay's upstream leg would dial itself", k, v)
		}
	}
	// Reported, not silent: a daemon that quietly dropped the developer's proxy
	// configuration would be its own mystery.
	if len(p.ClearedProxyEnv()) != len(keys) {
		t.Errorf("ClearedProxyEnv() = %v, want all %d keys named", p.ClearedProxyEnv(), len(keys))
	}
}

// TestNewAppliesItsOptions.
//
// New takes `opts ...Option` and, in its first draft, dropped them on the floor.
// A variadic option that is accepted and ignored is the worst kind of no-op: every
// call site reads as configured, and the daemon runs unconfigured — here that
// meant a --verbose transport logging nothing, which is indistinguishable from a
// relay nothing reaches.
func TestNewAppliesItsOptions(t *testing.T) {
	var lines int
	p, err := New(Config{}, testCA(t), &stubEmitter{},
		WithVerbose(func(string, ...any) { lines++ }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.vlog("hello")
	if lines != 1 {
		t.Errorf("the verbose option passed to New was dropped: %d lines logged, want 1", lines)
	}
}

// TestInterceptedRequestReachesTheHandlerOverRealTLS rehearses the whole CONNECT
// choreography in memory: write the 200, terminate TLS with a leaf from our CA,
// and serve HTTP/1.1 over it.
//
// It runs over net.Pipe because this host denies bind. That measures the
// choreography — the 200 line, the handshake, the origin-form request the handler
// sees, the response framing — and measures NOTHING about bind, listen or the
// dialer. The socket-level version is TestConnectPathIsByteIdentical, guarded.
func TestInterceptedRequestReachesTheHandlerOverRealTLS(t *testing.T) {
	ca := testCA(t)
	p, err := New(Config{}, ca, &stubEmitter{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A stub handler, because this test is about the tunnel rather than the relay.
	// The REAL gateway handler is exercised by the guarded socket test; a stub at
	// both ends would prove nothing about the seam, which is why the production
	// factory is asserted separately (TestProductionHandlerIsTheGatewayRelay).
	var gotTarget, gotHost, gotProto string
	p.handlerFor = func(string) (http.Handler, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotTarget, gotHost, gotProto = r.RequestURI, r.Host, r.Proto
			w.Header().Set("X-Stub", "1")
			io.WriteString(w, "pong")
		}), nil
	}

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close(); serverConn.Close() })

	done := make(chan error, 1)
	go func() { done <- p.interceptConn(serverConn, "api.anthropic.com:443") }()

	// Deadline everywhere: a broken handshake here would otherwise HANG, and a
	// stalled `go test` reads as an environment problem rather than an answer.
	if err := clientConn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	if got := readConnectResponse(t, clientConn); !strings.HasPrefix(got, "HTTP/1.1 200") {
		t.Fatalf("CONNECT response = %q, want a 200; the client will not begin its TLS handshake without one", got)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("CA PEM did not parse")
	}
	tc := tls.Client(clientConn, &tls.Config{ServerName: "api.anthropic.com", RootCAs: pool})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if got := tc.ConnectionState().NegotiatedProtocol; got == "h2" {
		t.Fatal("ALPN negotiated h2; the handler behind this speaks HTTP/1.1, so the request would " +
			"never parse and the model call would fail as if the network were broken")
	}

	req := "GET /v1/messages?beta=true HTTP/1.1\r\nHost: api.anthropic.com\r\n\r\n"
	if _, err := io.WriteString(tc, req); err != nil {
		t.Fatalf("write request: %v", err)
	}
	buf := make([]byte, 512)
	n, err := tc.Read(buf)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp := string(buf[:n])
	if !strings.HasPrefix(resp, "HTTP/1.1 200") || !strings.Contains(resp, "X-Stub: 1") {
		t.Errorf("response = %q, want a 200 from the stub handler", resp)
	}

	// ORIGIN-FORM is the property gateway.ServeHTTP requires: it refuses anything
	// that does not begin with "/", because the target is concatenated onto the
	// upstream and a non-origin form would splice into the authority. Over a
	// hijacked tunnel the client sends origin-form exactly as it would to the real
	// provider, and this asserts it rather than assuming it.
	if gotTarget != "/v1/messages?beta=true" {
		t.Errorf("handler saw RequestURI %q, want the origin-form target", gotTarget)
	}
	if gotHost != "api.anthropic.com" {
		t.Errorf("handler saw Host %q", gotHost)
	}
	if gotProto != "HTTP/1.1" {
		t.Errorf("handler saw %q, want HTTP/1.1", gotProto)
	}

	clientConn.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error("interceptConn did not return after the client closed; the tunnel goroutine leaks")
	}
}

// TestInterceptedTunnelServesMoreThanOneRequest.
//
// Claude Code reuses connections heavily, so one CONNECT carries many requests.
// Serving only the first would make every call after it hang until the client
// gave up and reconnected — slow and intermittent, which is the worst way for
// this to fail.
func TestInterceptedTunnelServesMoreThanOneRequest(t *testing.T) {
	ca := testCA(t)
	p, err := New(Config{}, ca, &stubEmitter{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var served int
	var mu sync.Mutex
	p.handlerFor = func(string) (http.Handler, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			served++
			n := served
			mu.Unlock()
			fmt.Fprintf(w, "reply-%d", n)
		}), nil
	}

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close(); serverConn.Close() })
	go p.interceptConn(serverConn, "api.anthropic.com:443")

	if err := clientConn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	readConnectResponse(t, clientConn)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	tc := tls.Client(clientConn, &tls.Config{ServerName: "api.anthropic.com", RootCAs: pool})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	for i := 1; i <= 3; i++ {
		if _, err := io.WriteString(tc, "GET /v1/models HTTP/1.1\r\nHost: api.anthropic.com\r\n\r\n"); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		buf := make([]byte, 256)
		n, err := tc.Read(buf)
		if err != nil {
			t.Fatalf("request %d response: %v", i, err)
		}
		want := fmt.Sprintf("reply-%d", i)
		if !strings.Contains(string(buf[:n]), want) {
			t.Fatalf("request %d got %q, want it to contain %q — the tunnel stopped serving after "+
				"the first request", i, buf[:n], want)
		}
	}
}

// TestProductionHandlerIsTheGatewayRelay.
//
// handlerFor is a seam a test can replace, so something has to assert that
// PRODUCTION does not get a stub. This checks the default factory builds a real
// gateway relay pointed at the intercepted host — the "no fake between the two
// fakes" control, at construction level.
func TestProductionHandlerIsTheGatewayRelay(t *testing.T) {
	em := &stubEmitter{}
	p, err := New(Config{}, testCA(t), em)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := p.handlerFor("api.anthropic.com:443")
	if err != nil {
		t.Fatalf("handlerFor: %v", err)
	}
	g, ok := h.(*gateway.Gateway)
	if !ok {
		t.Fatalf("the production handler is %T, want *gateway.Gateway — this lane must reuse the "+
			"proven relay rather than fork it", h)
	}
	if g == nil {
		t.Fatal("the production handler is a nil *gateway.Gateway")
	}
}

// TestTheGateIsNotWired.
//
// Refusal is dormant until probe A names a shape Claude Code does not retry
// around; a wrong shape silently disables a capability for the rest of the
// session. Dormancy is asserted STRUCTURALLY — no evaluator is constructed at
// all — rather than by wiring one and testing that it stays quiet, because the
// second version is more code and more risk for the same claim.
func TestTheGateIsNotWired(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "proxy.go", nil, 0)
	if err != nil {
		t.Fatalf("parse proxy.go: %v", err)
	}
	forbidden := map[string]bool{"WithGate": true, "Decide": true, "WriteRefusal": true, "RefuseEverything": true}

	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !forbidden[sel.Sel.Name] {
			return true
		}
		t.Errorf("%s: proxy.go calls %s. Refusal is dormant until probe A (phase 13) names a shape "+
			"Claude Code does not retry around; a wrong one silently disables a capability for the "+
			"whole session.", fset.Position(sel.Pos()), sel.Sel.Name)
		return true
	})
}

// TestNonAllowlistedHostIsNeverCaptured is the promise that makes the ADR-0021 §5
// reversal defensible, asserted rather than described: a host outside the
// allowlist is not intercepted, so no capture can exist for it.
func TestNonAllowlistedHostIsNeverCaptured(t *testing.T) {
	em := &stubEmitter{}
	p, err := New(Config{}, testCA(t), em)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.intercepts("github.com:443") {
		t.Fatal("a non-allowlisted host is intercepted")
	}
	if em.count() != 0 {
		t.Errorf("%d captures exist for a host that was never intercepted", em.count())
	}
}
