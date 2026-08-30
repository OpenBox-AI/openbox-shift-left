// Package dialhook holds the relay's upstream TCP dial.
//
// It is a separate package for one reason: the dial has to be replaceable by
// tests in OTHER modules — the transport lane's CONNECT path lives in
// `transport/` and its end-to-end control lives in the CLI, and neither can reach
// an unexported variable in package gateway — while staying impossible to
// replace from production code anywhere.
//
// `internal/` under gateway/ is what buys the second half: no module outside this
// one can import this package at all. The test-only mutator that DOES reach it
// lives in gateway/gatewaytest, which carries its own tripwire against a
// non-test importer.
//
// Why a swappable dial rather than a swappable Transport, which would be less
// machinery: the relay builds the repository's only hand-tuned http.Transport —
// DisableCompression, ForceAttemptHTTP2, the redirect rule, the idle pool — and
// those settings are exactly what the byte-identity assertions exist to prove.
// Substituting the Transport would bypass every one of them and leave the
// assertions describing a transport the product does not use.
package dialhook

import (
	"context"
	"net"
	"time"
)

// UpstreamDialContext is the dial the relay's transport uses.
//
// Production never assigns it. It is read PER DIAL rather than captured when the
// relay is constructed, so a test that swaps it after building its gateway still
// gets the swap — the alternative makes correctness depend on construction order,
// which is the kind of ordering rule that is obeyed until it is not and then
// fails as "the upstream was unreachable".
var UpstreamDialContext = (&net.Dialer{
	Timeout:   10 * time.Second,
	KeepAlive: 30 * time.Second,
}).DialContext

// Dial is the indirection the relay installs on its Transport.
func Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return UpstreamDialContext(ctx, network, addr)
}
