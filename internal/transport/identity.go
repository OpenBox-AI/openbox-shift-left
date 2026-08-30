// Package transport is the in-path model-call lane (that decision, `:proxy:`).
package transport

import (
	"net/http"

	"github.com/elazarl/goproxy"
)

// NewIdentityProxy returns a goproxy configured to forward byte-identically.
//   - KeepAcceptEncoding: goproxy's RemoveProxyHeaders deletes the client's
//     Accept-Encoding header by default (proxy.go, RemoveProxyHeaders).
//   - Tr.DisableCompression: with Accept-Encoding absent, Go's own Transport
//     adds `gzip` on the caller's behalf and transparently decompresses the
//     reply; so the bytes reaching the client are not the bytes the provider
//     sent.
//   - PreventCanonicalization: header names pass through as the client wrote
//     them instead of being RFC-canonicalized.
func NewIdentityProxy() *goproxy.ProxyHttpServer {
	p := goproxy.NewProxyHttpServer()
	p.KeepAcceptEncoding = true
	p.PreventCanonicalization = true

	p.Tr = &http.Transport{
		Proxy:              http.ProxyFromEnvironment,
		DisableCompression: true,
		ForceAttemptHTTP2:  false,
	}
	return p
}
