// Package transport is the in-path model-call lane (ADR-0022, `:proxy:`).
//
// It grew out of a goproxy spike whose only output was byte-identical
// forwarding. That is no longer what this is: the lane now carries the host
// allowlist (allowlist.go), the name-constrained project CA (ca.go), and the
// hijack in proxy.go that hands an intercepted connection to gateway.Gateway,
// which owns every byte and every piece of evidence. Capture is REQUIRED at
// construction — there is no state in which this lane relays without recording.
//
// The relay is REUSED, not forked. A second implementation of byte-identical
// forwarding, per-chunk SSE, the fingerprint-before-redact ordering and the 64KB
// cap would sit on the enforcement path, which is where this repo's copy-paste
// original sin already happened once.
//
// DEPENDENCY BOUNDARY. This subtree's imports are held to an allowlist in
// internal/depguard, both external and repo-local (ADR-0023 as amended by
// ADR-0024). Adding an import outside it fails there first, which is the
// point — widening the list to make an import pass inverts the ADR's
// reasoning. This comment is the signpost; depguard is the enforcement.
package transport

import (
	"net/http"

	"github.com/elazarl/goproxy"
)

// NewIdentityProxy returns a goproxy configured to forward byte-identically.
//
// Three non-default settings are required, and the plan's spike criteria named
// only one of them. Each is here because a default silently changes the bytes:
//
//   - KeepAcceptEncoding: goproxy's RemoveProxyHeaders DELETES the client's
//     Accept-Encoding header by default (proxy.go, RemoveProxyHeaders). Note the
//     direction — the plan anticipated goproxy *injecting* `gzip`; v1.9.0 strips
//     what the client actually sent. Either way the provider then sees a
//     different request than the client made.
//   - Tr.DisableCompression: with Accept-Encoding absent, Go's own Transport adds
//     `gzip` on the caller's behalf and transparently decompresses the reply — so
//     the bytes reaching the client are not the bytes the provider sent. This is
//     the same setting, for the same reason, that gateway/proxy.go sets on its own
//     transport; goproxy's own field doc points at it explicitly.
//   - PreventCanonicalization: header NAMES pass through as the client wrote them
//     instead of being RFC-canonicalized. Anthropic's API is not known to care,
//     but "forward verbatim" is the property this lane exists to hold, and a
//     silently rewritten name is a rewritten byte.
//
// What is deliberately NOT set: AllowHTTP2 stays false. HTTP/2 in an in-path
// relay is its own gate (frame-level rewriting, not byte-level) and the gateway
// lane has never claimed it either.
//
// Header ORDER is not preserved and cannot be: net/http models headers as a map,
// so no Go proxy can hold their order — and the gateway's own identity suite
// asserts presence, values and absence-of-additions rather than order, for
// exactly this reason. The plan's "header order preserved" criterion is
// unachievable as written; this is the closest true statement.
func NewIdentityProxy() *goproxy.ProxyHttpServer {
	p := goproxy.NewProxyHttpServer()
	p.KeepAcceptEncoding = true
	p.PreventCanonicalization = true

	// The transport is replaced wholesale so DisableCompression is set on the one
	// actually used, and so the upstream leg is stated here rather than inherited.
	//
	// A note against a misreading, because the name invites it: goproxy's default
	// transport uses a variable called `tlsClientSkipVerify`
	// (proxy.go:176 → certs.go:25), which sounds like verification is off. It is
	// `&tls.Config{}` — an EMPTY config, so InsecureSkipVerify is false and
	// verification is stock. Checked, because an in-path relay that skipped
	// upstream verification would be a downgrade this product cannot ship, and
	// the name alone would have justified a wrong "fix".
	p.Tr = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// Both halves of the compression rule, as above.
		DisableCompression: true,
		ForceAttemptHTTP2:  false,
	}
	return p
}
