// Package dialhook holds the relay's upstream TCP dial.
package dialhook

import (
	"context"
	"net"
	"time"
)

// UpstreamDialContext is the dial the relay's transport uses. Production never
// assigns it.
var UpstreamDialContext = (&net.Dialer{
	Timeout:   10 * time.Second,
	KeepAlive: 30 * time.Second,
}).DialContext

// Dial is the indirection the relay installs on its Transport.
func Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return UpstreamDialContext(ctx, network, addr)
}
