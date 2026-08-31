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
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/test/bufconn"
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
	registry    = map[string]*bufconn.Listener{}
	seq         atomic.Uint64
	installOnce sync.Once
	installErr  error
)

// bufSize is the capacity of each direction of a connection. It is flow
// control, not a cliff: a body past it makes the writer wait for the reader,
// and both halves of an HTTP exchange are pumped concurrently, so it resolves.
// 1 MiB is above every bound the suite can put on the wire -- MaxRedactBody is
// 512 KiB, maxThinkingBytes 256 KiB, and the client itself reads at most
// io.LimitReader(resp.Body, 1<<20) -- so nothing in it ever has to wait.
const bufSize = 1 << 20

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
				return l.DialContext(ctx)
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
	lis    *bufconn.Listener
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
	lis := bufconn.Listen(bufSize)

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
		return l.DialContext(ctx)
	case known:
		return nil, fmt.Errorf("memhttptest: dial %s: connection refused (server closed)", addr)
	}
	// A test asserting dial latency would be measuring the wrong constant here;
	// gateway's production dialer is 10s/30s; so do not write one against this.
	return (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, addr)
}
