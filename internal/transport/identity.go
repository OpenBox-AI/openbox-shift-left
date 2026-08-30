// Package transport is the in-path model-call lane (that decision, `:proxy:`).
package transport

import (
	"net/http"

	"github.com/elazarl/goproxy"
)

// NewIdentityProxy returns a goproxy configured to forward byte-identically.
// Each is here because a default silently changes the bytes:
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
