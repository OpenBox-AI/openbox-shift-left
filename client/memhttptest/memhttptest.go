// Package memhttptest serves HTTP over in-memory pipes, for hosts that deny bind.
//
// It exists because some sandboxes refuse every `bind` syscall — TCP and unix
// alike — which makes `httptest.NewServer` PANIC. A panic in one test kills the
// whole test binary, so every test declared after the panicking one never runs
// and reports neither pass nor fail. That failure mode is silent in exactly the
// wrong direction: the package looks like it has two failures when in truth
// most of its tests produced no verdict at all.
//
// What this preserves, and it is the point: the REAL net/http client and the
// REAL net/http server still run, so the request is genuinely marshalled,
// framed, transmitted and parsed. The handler observes the same outbound bytes
// it would observe behind a TCP socket. The only substitution is the net.Conn
// underneath — a net.Pipe instead of a socket. So an assertion on the received
// body is evidence about the payload, exactly as it was before; it is NOT
// evidence about bind, listen, TLS or the dialer, and no test should read it
// that way.
//
// One fidelity gap to know about before writing a test against this: writes are
// BUFFERED (see bufferedPipe), so a write error surfaces one write late and an
// error on the final write before Close is discarded. Behaviour that depends on
// a write deadline firing against a stalled reader — gateway's writeIdleTimeout,
// for one — cannot be regression-tested here. Guard such a test with
// RequireBind instead.
//
// Test-only. Nothing in production imports this package, and
// TestMemhttptestStaysTestOnly holds that.
package memhttptest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// TB is the slice of *testing.T these helpers use.
//
// Declared locally rather than importing `testing`, because this package lives
// in the shipped `client` module and importing `testing` from a non-test package
// registers the test flags on any binary that links it. *testing.T satisfies it
// structurally, so call sites just pass t.
type TB interface {
	Helper()
	Cleanup(func())
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
}

// Servers present themselves as loopback on a synthetic port.
//
// The host is a literal 127.0.0.1 rather than a made-up name because the code
// under test frequently VALIDATES that its target is loopback (gateway's
// Config.Validate, the CLI's non-loopback flag). A URL like
// "http://memhttp.invalid" would be rejected by the very check the test exists
// to exercise, and the test would then pass or fail for the wrong reason.
//
// The ports are never bound, so they cannot collide with anything; the number
// only has to be unique within the process so two servers stay distinguishable.
const basePort = 45000

var (
	mu          sync.RWMutex
	registry    = map[string]*listener{}
	seq         atomic.Uint64
	installOnce sync.Once
	installErr  error
)

// install swaps http.DefaultTransport for a clone that consults the registry
// before dialing.
//
// The clone matters: every other setting on the default transport is preserved,
// so a test's behaviour changes only for the synthetic addresses this package
// handed out. Unknown addresses fall through to the original dialer, which keeps
// "this host is unreachable" tests failing the way they always did.
//
// The default transport is a process-wide global and this never restores it.
// That is deliberate — restoring it per-server would race tests that share the
// process — and it is safe because the installed transport is behaviour-
// preserving for any address not in the registry.
func install() error {
	installOnce.Do(func() {
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			installErr = fmt.Errorf("memhttptest: http.DefaultTransport is %T, not *http.Transport", http.DefaultTransport)
			return
		}
		next := base.Clone()
		fallback := next.DialContext
		if fallback == nil {
			fallback = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		}
		next.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			mu.RLock()
			l, known := registry[addr]
			mu.RUnlock()
			switch {
			case l != nil:
				return l.dial(ctx)
			case known:
				// Registered once, now closed. Refuse rather than fall through.
				return nil, fmt.Errorf("memhttptest: dial %s: connection refused (server closed)", addr)
			}
			return fallback(ctx, network, addr)
		}
		http.DefaultTransport = next
	})
	return installErr
}

// Server is the drop-in for httptest.Server: URL, Client and Close, same shapes.
type Server struct {
	URL string

	addrs  []string
	lis    *listener
	srv    *http.Server
	closed sync.Once
}

// NewServer starts a server on an in-memory listener and registers its
// synthetic address, so http.DefaultTransport — and therefore any client that
// did not set its own Transport — reaches it.
func NewServer(t TB, h http.Handler) *Server {
	t.Helper()
	if err := install(); err != nil {
		t.Fatalf("memhttptest: %v", err)
	}
	port := basePort + int(seq.Add(1))
	// All three spellings of loopback map to this one server: code under test
	// may normalise "127.0.0.1" to "localhost" (or vice versa) before dialing,
	// and a miss would fall through to a real dial and fail confusingly.
	addrs := []string{
		fmt.Sprintf("127.0.0.1:%d", port),
		fmt.Sprintf("localhost:%d", port),
		fmt.Sprintf("[::1]:%d", port),
	}
	lis := &listener{conns: make(chan net.Conn), done: make(chan struct{})}

	mu.Lock()
	for _, a := range addrs {
		registry[a] = lis
	}
	mu.Unlock()

	s := &Server{
		URL:   fmt.Sprintf("http://127.0.0.1:%d", port),
		addrs: addrs,
		lis:   lis,
		srv:   &http.Server{Handler: h},
	}
	go func() { _ = s.srv.Serve(lis) }()
	t.Cleanup(s.Close)
	return s
}

// Client returns a client that reaches this server. It is the default transport,
// which the registry has already taught how to find us — returned as its own
// method only so call sites can mirror httptest.Server.Client().
func (s *Server) Client() *http.Client {
	return &http.Client{Transport: http.DefaultTransport}
}

// Close is idempotent, because call sites use both `defer srv.Close()` and
// `t.Cleanup(srv.Close)` and NewServer registers the cleanup itself.
func (s *Server) Close() {
	s.closed.Do(func() {
		mu.Lock()
		for _, a := range s.addrs {
			// Leave a REFUSING sentinel rather than deleting the entry. Deleting
			// it makes a later dial of this URL fall through to a REAL dial of
			// 127.0.0.1:<synthetic port> — which on a bind-capable machine can
			// reach an unrelated local service and, worst case, POST a signed
			// governance payload at it. "Connection refused" is both safer and a
			// truer emulation of a stopped server.
			registry[a] = nil
		}
		mu.Unlock()
		s.lis.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(ctx)
	})
}

// listener hands out one end of a net.Pipe per dial. No syscall is involved.
type listener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, errors.New("memhttptest: listener closed")
	}
}

func (l *listener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *listener) Addr() net.Addr { return addr{} }

func (l *listener) dial(ctx context.Context) (net.Conn, error) {
	client, server := bufferedPipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
		_ = client.Close()
		_ = server.Close()
		return nil, errors.New("memhttptest: connection refused (server closed)")
	case <-ctx.Done():
		_ = client.Close()
		_ = server.Close()
		return nil, ctx.Err()
	}
}

type addr struct{}

func (addr) Network() string { return "mem" }
func (addr) String() string  { return "memhttp" }

// RequireBind skips the test when this host denies bind.
//
// Some tests genuinely need a real socket: the ones that compile the binary and
// run it as a CHILD process cannot reach an in-memory listener, because the pipe
// lives in the parent's address space. For those an in-memory server is not a
// substitute, and a skip naming the reason is more honest than a failure that
// reads as a defect in the code under test.
func RequireBind(t TB) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("memhttptest: host denies bind (%v); this test needs a real socket a child process can dial", err)
	}
	_ = l.Close()
}

// RequireResolvableHost skips the test when the host cannot be resolved.
//
// A few checks assert that a REACHABLE provider URL is not described as failing
// safe — which only means anything if the name resolves. In a sandbox with no
// DNS the lookup fails, the check correctly reports "unreachable", and the test
// then fails for the environment's reason rather than the code's. Skipping names
// that difference instead of hiding it.
func RequireResolvableHost(t TB, host string) {
	t.Helper()
	if _, err := net.LookupHost(host); err != nil {
		t.Skipf("memhttptest: %s does not resolve here (%v); this test needs real DNS", host, err)
	}
}

// DialContext resolves a registered synthetic address to its in-memory pipe,
// falling back to a real dial for anything else.
//
// Exported for the tests whose code under test builds its OWN http.Transport and
// therefore never consults http.DefaultTransport. Those transports must keep
// every other setting they have — in the gateway's case DisableCompression and
// the HTTP/2 attempt are the very things under test — so the call site replaces
// only the dialer.
func DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	mu.RLock()
	l, known := registry[addr]
	mu.RUnlock()
	switch {
	case l != nil:
		return l.dial(ctx)
	case known:
		return nil, fmt.Errorf("memhttptest: dial %s: connection refused (server closed)", addr)
	}
	// The fallback's timeouts are this package's, not any caller's. A test
	// asserting DIAL LATENCY would be measuring the wrong constant here —
	// gateway's production dialer is 10s/30s — so do not write one against this.
	return (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, addr)
}

// bufferedPipe is net.Pipe with the send-side buffering a real socket has.
//
// A raw net.Pipe is fully synchronous: a Write blocks until the peer Reads. That
// INVERTS an ordering every HTTP test quietly depends on. Over a socket the
// handler's final Write lands in the kernel buffer and returns, so the handler
// runs its post-response work — logging the outcome, recording a metric — while
// the client is still reading. Over a raw pipe the handler's Write cannot return
// until the client has consumed the bytes, so that work now happens strictly
// AFTER the client's read completes, and any test that reads the response and
// then inspects what the handler recorded becomes a race it did not used to be.
//
// Buffering the send side restores the socket's decoupling. It makes the
// substitution MORE faithful, not less: the thing being emulated is a buffered
// transport.
func bufferedPipe() (net.Conn, net.Conn) {
	a, b := net.Pipe()
	return newBufferedConn(a), newBufferedConn(b)
}

// bufferedConn buffers writes and drains them to the underlying pipe from its
// own goroutine. Reads, deadlines and addresses pass straight through.
type bufferedConn struct {
	net.Conn

	mu       sync.Mutex
	cond     *sync.Cond
	pending  []byte
	closing  bool
	writeErr error
	drained  chan struct{}
}

func newBufferedConn(c net.Conn) *bufferedConn {
	bc := &bufferedConn{Conn: c, drained: make(chan struct{})}
	bc.cond = sync.NewCond(&bc.mu)
	go bc.pump()
	return bc
}

func (bc *bufferedConn) Write(p []byte) (int, error) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	if bc.writeErr != nil {
		return 0, bc.writeErr
	}
	if bc.closing {
		return 0, net.ErrClosed
	}
	bc.pending = append(bc.pending, p...)
	bc.cond.Signal()
	return len(p), nil
}

func (bc *bufferedConn) pump() {
	defer close(bc.drained)
	for {
		bc.mu.Lock()
		for len(bc.pending) == 0 && !bc.closing {
			bc.cond.Wait()
		}
		chunk := bc.pending
		bc.pending = nil
		closing := bc.closing
		bc.mu.Unlock()

		if len(chunk) > 0 {
			if _, err := bc.Conn.Write(chunk); err != nil {
				bc.mu.Lock()
				bc.writeErr = err
				bc.pending = nil
				bc.mu.Unlock()
				return
			}
		}
		if closing && len(chunk) == 0 {
			return
		}
	}
}

// Close drains what is already buffered before closing the pipe, so a response
// written just before the handler returns is not lost. The wait is bounded: a
// peer that stopped reading must not wedge a test.
func (bc *bufferedConn) Close() error {
	bc.mu.Lock()
	if bc.closing {
		bc.mu.Unlock()
		return nil
	}
	bc.closing = true
	bc.cond.Signal()
	bc.mu.Unlock()

	select {
	case <-bc.drained:
	case <-time.After(2 * time.Second):
	}
	return bc.Conn.Close()
}
