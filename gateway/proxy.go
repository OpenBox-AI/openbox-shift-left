// Package gateway is the OpenBox local gateway: an Anthropic-Messages-format
// reverse proxy that runs on the developer's own machine, substitutes for the
// provider base URL, and forwards every request onward byte-identically.
//
// Two invariants define it, and both are tested rather than described:
//
//   - Inspect without modifying. Forwarded bytes are the received bytes, the
//     Authorization header included. The gateway resolves, stores and injects no
//     credential of its own -- the developer's own credential relays untouched,
//     which is why this binary holds no provider secret anywhere.
//   - Never buffer a RESPONSE. Claude Code counts every relayed byte against a
//     180s watchdog and SSE ping/comment lines are what keep a long thinking
//     pause alive, so the response is teed straight through with a flush per
//     chunk. The request direction is deliberately the opposite: it IS buffered,
//     because the exact bytes have to be re-readable -- phase 05 keeps a copy to
//     capture, and net/http can only auto-retry a stale pooled connection when
//     GetBody can replay the body. One-way streaming is a property of the
//     response, not of the relay.
//
// The relay is hand-rolled, and the reason needs stating precisely because the
// obvious version of it is wrong. It is NOT that httputil.ReverseProxy cannot be
// made to behave: the legacy Director path does append X-Forwarded-For (measured
// -- the identity test was first run against NewSingleHostReverseProxy and failed
// on exactly that, plus an injected Accept-Encoding: gzip), but the modern
// Rewrite hook deletes X-Forwarded-* and re-adds them only on an explicit
// SetXForwarded, and its flushInterval already auto-selects immediate flush for
// text/event-stream. A Rewrite-based proxy with this same Transport would satisfy
// the two invariants above.
//
// What it would not give is the thing phase 05 needs: a tee of BOTH directions
// under this package's own control, with the request copy re-readable. That, plus
// the plan's own mitigation for ping-stripping ("hand-roll the SSE relay"), is
// what buys the ~90 lines. The Transport defaults still have to be defeated
// either way -- Accept-Encoding: gzip is injected and transparently decompressed
// unless DisableCompression is set, and a re-sent body with no declared length
// flips the framing to chunked.
package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

// hopByHopHeaders describe a single connection rather than the message, so a
// relay drops them instead of forwarding them. Everything else -- including
// every header this version has never heard of -- passes through untouched: an
// allowlist would need editing every time the provider adds a field, and a
// gateway that must be edited to stay correct is a gateway that silently breaks.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// relayBufferSize is the tee chunk, matching net/http.Transport's own read
// buffer -- the ceiling on what one upstream Read can return when nothing is
// already buffered.
//
// It is NOT what protects SSE pings. stream() flushes buf[:n] after every
// non-empty Read, so a 50-byte ping relays as 50 bytes at any buffer size; the
// flush-per-read is the mechanism, not the size. Growing this would cut syscalls
// on a large bursty body and cost nothing in latency, but that is an unmeasured
// optimization, so the value stays where the stdlib puts it.
const relayBufferSize = 4 << 10

// maxRequestBody bounds the inbound read, matching the convention every other
// externally-controlled body read in this repo already follows (maxHookPayload,
// maxTranscriptBytes, maxRolloutBytes, maxFindingsDelta). It is set at the
// largest of those precedents because a model request legitimately carries file
// contents and base64 media, and refusing real traffic is the worse failure.
//
// The bound refuses; it never truncates. A relay that forwarded a short body
// would corrupt the request while reporting success, which is the exact silent
// mutation this package exists to prevent — so an over-cap request is answered
// 413 and nothing is forwarded at all.
const maxRequestBody = 64 << 20 // 64 MiB

// writeIdleTimeout bounds a single write to the local caller. Generous on
// purpose: on loopback, a client that has not drained a pending chunk for this
// long is stuck, not slow.
const writeIdleTimeout = 2 * time.Minute

// Gateway relays requests to the configured upstream. It is safe for concurrent
// use and holds no credential state.
type Gateway struct {
	upstream string
	client   *http.Client
	// maxBody is maxRequestBody in production. It is a field so a test can drive
	// the over-cap path without allocating 64 MiB.
	maxBody int64
}

// New validates the configuration and returns the relay.
func New(cfg Config) (*Gateway, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Gateway{
		upstream: cfg.Upstream,
		maxBody:  maxRequestBody,
		client: &http.Client{
			// A redirect must reach the client as a redirect. Following one
			// would forward the developer's credential to whatever host the
			// response named, and would hide from Claude Code's model discovery
			// that the base URL did not answer directly.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			// No Client.Timeout: a streamed completion legitimately runs for
			// minutes, and an overall deadline would abort it mid-stream.
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				ForceAttemptHTTP2: true,
				MaxIdleConns:      100,
				// Every request from this relay goes to the one upstream host, so
				// without this the effective idle pool is
				// DefaultMaxIdleConnsPerHost (2), not the 100 above -- and the
				// third concurrent call pays a fresh TCP+TLS handshake. Mostly
				// masked under HTTP/2 multiplexing, which is exactly why it needs
				// setting: behind a corporate proxy the connection can downgrade
				// to HTTP/1.1, where it bites hardest.
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
				// Both halves of the compression rule. Without this the
				// transport advertises gzip on the client's behalf and then
				// decompresses the reply, so the bytes the client receives are
				// not the bytes the provider sent.
				DisableCompression: true,
			},
		},
	}, nil
}

// ServeHTTP relays the request upstream.
//
// There is no path allowlist. `POST /v1/messages`, `/v1/messages/count_tokens`,
// `GET /v1/models` and `HEAD /api/hello` are all served by this one relay, and
// so is anything the provider adds next -- being a transparent stand-in is what
// keeps this forward-compatible without a release.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Read the body once. The exact bytes are what gets forwarded, and knowing
	// the length is what keeps the framing identical instead of chunked.
	//
	// Bounded per maxRequestBody. Note the asymmetry with capture: phase 05 caps
	// the COPY it keeps at 64KB, while the bytes that get forwarded are relayed
	// whole or not at all.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, g.maxBody))
	if err != nil {
		var toolarge *http.MaxBytesError
		if errors.As(err, &toolarge) {
			// Refuse, never forward a partial body.
			g.relayError(w, http.StatusRequestEntityTooLarge, "request body exceeds the gateway relay limit")
			return
		}
		g.relayError(w, http.StatusBadRequest, "reading request body")
		return
	}

	// Match on path, not URL, and carry the query through: requests arrive as
	// /v1/messages?beta=true.
	//
	// r.RequestURI, NOT r.URL.RequestURI(): the latter rebuilds the target from
	// the parsed URL, and for a path carrying a percent-escape Go would not have
	// chosen itself (%2E, %41) EscapedPath falls back to re-encoding -- so the
	// bytes forwarded would differ from the bytes received. r.RequestURI is the
	// unmodified request-target off the request line.
	target := g.upstream + r.RequestURI

	outbound, err := http.NewRequestWithContext(r.Context(), r.Method, target, nil)
	if err != nil {
		g.relayError(w, http.StatusBadGateway, "cannot form upstream request")
		return
	}
	if len(body) > 0 {
		outbound.Body = io.NopCloser(bytes.NewReader(body))
		outbound.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}
	// Declared explicitly so the transport does not fall back to chunked.
	outbound.ContentLength = int64(len(body))
	copyHeaders(outbound.Header, r.Header)

	resp, err := g.client.Do(outbound)
	if err != nil {
		// A client that hung up mid-request is not a gateway failure; there is
		// nobody left to answer.
		if errors.Is(err, context.Canceled) {
			return
		}
		g.relayError(w, http.StatusBadGateway, "upstream unreachable")
		return
	}
	defer resp.Body.Close()

	// Response headers first, unmodified, then the status, then the stream. Once
	// WriteHeader has run nothing may be added, which is what keeps an upstream
	// error body from acquiring an OpenBox envelope.
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	g.stream(w, resp.Body)
}

// stream tees the response through with a flush per chunk. Buffer-then-forward
// is forbidden here: Claude Code's byte watchdog cannot tell a buffered relay
// from a stalled provider.
func (g *Gateway) stream(w http.ResponseWriter, src io.Reader) {
	ctl := http.NewResponseController(w)
	buf := make([]byte, relayBufferSize)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			// An IDLE deadline, reset per successful write -- not a total
			// duration cap, which would abort a legitimately long stream. It
			// bounds the one case that otherwise never ends: a local caller that
			// stops reading without closing, leaving w.Write blocked on a full
			// send buffer with nothing to unblock it. This process is a
			// supervised daemon, so that goroutine and its upstream connection
			// would leak for the process lifetime rather than the request's.
			// A ResponseWriter that cannot take a deadline is left alone.
			_ = ctl.SetWriteDeadline(time.Now().Add(writeIdleTimeout))
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			// Ignored deliberately: a ResponseWriter that cannot flush (HTTP/2
			// handles it itself, or the client is gone) is not a reason to abort
			// a stream that is otherwise being delivered.
			_ = ctl.Flush()
		}
		if readErr != nil {
			return
		}
	}
}

// relayError answers when the relay itself could not proceed. It never wraps an
// upstream response -- those are forwarded verbatim -- and it never echoes
// request detail, because the developer's live credential transits every request.
func (g *Gateway) relayError(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"type":"error","error":{"type":"openbox_gateway_error","message":%q}}`, reason)
}

// copyHeaders copies every header except the hop-by-hop set. Host is absent from
// a server-side header map by construction (net/http keeps it on Request.Host),
// and it is the one field the relay must legitimately change: the upstream needs
// its own name for TLS SNI and routing.
//
// The static set is not the whole rule. RFC 7230 6.1 makes any field NAMED INSIDE
// a Connection value hop-by-hop for that message too, so those are dropped as
// well -- forwarding one would hand the upstream a directive that described only
// the local hop.
func copyHeaders(dst, src http.Header) {
	perMessage := connectionNamedHeaders(src)
	for name, values := range src {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if hopByHopHeaders[canonical] || perMessage[canonical] {
			continue
		}
		dst[name] = append([]string(nil), values...)
	}
}

// connectionNamedHeaders returns the field names a Connection header declares as
// connection-scoped for this message.
func connectionNamedHeaders(src http.Header) map[string]bool {
	named := map[string]bool{}
	for _, value := range src["Connection"] {
		for _, part := range strings.Split(value, ",") {
			if name := strings.TrimSpace(part); name != "" {
				named[textproto.CanonicalMIMEHeaderKey(name)] = true
			}
		}
	}
	return named
}
