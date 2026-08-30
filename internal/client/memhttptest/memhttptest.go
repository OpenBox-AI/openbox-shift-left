// Package memhttptest serves HTTP over in-memory pipes, for hosts that deny
// bind. A panic in one test kills the whole test binary, so every test
// declared after the panicking one never runs and reports neither pass nor
// fail. Behaviour that depends on a write deadline firing against a stalled
// reader; gateway's writeIdleTimeout, for one; cannot be regression-tested
// here.
//   - A child process.
//   - Code that builds ITS OWN http.Transport.
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
type TB interface {
	Helper()
	Cleanup(func())
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
}

// basePort the ports are never bound, so they cannot collide with anything;
// the number only has to be unique within the process so two servers stay
// distinguishable.
const basePort = 45000

var (
	mu          sync.RWMutex
	registry    = map[string]*listener{}
	seq         atomic.Uint64
	installOnce sync.Once
	installErr  error
)

// install the default transport is a process-wide global and this never
// restores it.
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
				return nil, fmt.Errorf("memhttptest: dial %s: connection refused (server closed)", addr)
			}
			return fallback(ctx, network, addr)
		}
		http.DefaultTransport = next
	})
	return installErr
}

// Server is the drop-in for httptest.Server: URL, Client and Close, same
// shapes.
type Server struct {
	URL string

	addrs  []string
	lis    *listener
	srv    *http.Server
	closed sync.Once
}

// NewServer starts a server on an in-memory listener and registers its
// synthetic address, so http.DefaultTransport; and therefore any client that
// did not set its own Transport; reaches it.
func NewServer(t TB, h http.Handler) *Server {
	t.Helper()
	if err := install(); err != nil {
		t.Fatalf("memhttptest: %v", err)
	}
	port := basePort + int(seq.Add(1))
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

// Client returns a client that reaches this server.
func (s *Server) Client() *http.Client {
	return &http.Client{Transport: http.DefaultTransport}
}

// Close is idempotent, because call sites use both `defer srv.Close()` and
// `t.Cleanup(srv.Close)` and NewServer registers the cleanup itself.
func (s *Server) Close() {
	s.closed.Do(func() {
		mu.Lock()
		for _, a := range s.addrs {
			registry[a] = nil
		}
		mu.Unlock()
		s.lis.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(ctx)
	})
}

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

// RequireBind skips the test when this host denies bind. Some tests genuinely
// need a real socket: the ones that compile the binary and run it as a child
// process cannot reach an in-memory listener, because the pipe lives in the
// parent's address space.
func RequireBind(t TB) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("memhttptest: host denies bind (%v); this test needs a real socket a child process can dial", err)
	}
	_ = l.Close()
}

// RequireResolvableHost skips the test when the host cannot be resolved.
func RequireResolvableHost(t TB, host string) {
	t.Helper()
	if _, err := net.LookupHost(host); err != nil {
		t.Skipf("memhttptest: %s does not resolve here (%v); this test needs real DNS", host, err)
	}
}

// DialContext resolves a registered synthetic address to its in-memory pipe,
// falling back to a real dial for anything else. Exported for the tests whose
// code under test builds its OWN http.Transport and therefore never consults
// http.DefaultTransport.
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
	// A test asserting dial latency would be measuring the wrong constant here;
	// gateway's production dialer is 10s/30s; so do not write one against this.
	return (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, addr)
}

func bufferedPipe() (net.Conn, net.Conn) {
	a, b := net.Pipe()
	return newBufferedConn(a), newBufferedConn(b)
}

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
