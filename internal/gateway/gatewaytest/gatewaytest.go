// Package gatewaytest exposes the relay's upstream dial to tests in other
// modules, and nothing else. Do not call it from a test that also calls
// t.Parallel, and do not use it in a test whose point is that an upstream is
// unreachable; a registry dialer could answer where the test needed a refusal,
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
