// Package gatewaytest exposes the relay's upstream dial to tests in other
// modules, and nothing else.
//
// It exists because of a specific gap this repository could not otherwise close.
// The transport lane terminates an intercepted CONNECT and serves the existing
// relay over it, and until now NO RESPONSE BODY had ever traversed that path:
// the control test runs on a host that denies bind, so its upstream was a refused
// loopback port. Swapping only the dial lets a real recorded exchange cross the
// real CONNECT, the real TLS handshake and the real relay on such a host.
//
// WHAT A SWAPPED RUN PROVES: the relay's own Transport settings are still in the
// path (a test asserting the client's Accept-Encoding arrived byte-identical is
// what holds that), the request and response bytes, the capture, the redaction,
// the caps, the spool.
//
// WHAT IT DOES NOT PROVE: bind, listen, the real dialer, TLS to a real socket, or
// write-deadline behaviour — an in-memory pipe's send side is buffered, so
// handler/client interleaving differs from a socket's. Those belong to the
// bind-guarded suites, which stay.
//
// The swap is PROCESS-GLOBAL. Do not call it from a test that also calls
// t.Parallel, and do not use it in a test whose point is that an upstream is
// unreachable — a registry dialer could answer where the test needed a refusal,
// and it would pass for the wrong reason.
package gatewaytest

import (
	"context"
	"net"
	"sync"

	"github.com/openbox-ai/openbox-shift-left/internal/gateway/internal/dialhook"
)

// TB is the testing surface this helper needs, declared structurally so the
// package does not import testing into a non-test file.
type TB interface {
	Helper()
	Cleanup(func())
}

var mu sync.Mutex

// SwapUpstreamDial replaces the relay's upstream dial for the duration of one
// test, restoring it on cleanup.
func SwapUpstreamDial(t TB, dial func(ctx context.Context, network, addr string) (net.Conn, error)) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	prev := dialhook.UpstreamDialContext
	dialhook.UpstreamDialContext = dial
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		dialhook.UpstreamDialContext = prev
	})
}
