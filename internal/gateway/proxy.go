// Package gateway is the OpenBox local gateway: an Anthropic-Messages-format
// reverse proxy that runs on the developer's own machine, substitutes for the
// provider base URL, and forwards every request onward byte-identically.
//
// Two rules govern the relay:
//   - Never buffer a response, so a stream reaches the client per chunk.
//   - Inspect without modifying.
//
// The request direction is deliberately the opposite: it is buffered, because
// the exact bytes have to be re-readable. Capture keeps a copy, and net/http can
// only auto-retry a stale pooled connection when GetBody can replay the body.
package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/gateway/internal/dialhook"
)

var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Proxy-Connection":    true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

const relayBufferSize = 4 << 10

// maxRequestBody the bound refuses; it never truncates.
const maxRequestBody = 64 << 20 // 64 MiB

// writeIdleTimeout generous on purpose: on loopback, a client that has not
// drained a pending chunk for this long is stuck, not slow.
const writeIdleTimeout = 2 * time.Minute

// Emitter receives the evidence one relayed call produced. A seam, so the
// gateway never builds a client of its own: the CLI wires the same client,
// auth and signing the hook path uses, and this package stays free of
// credential handling.
type Emitter interface {
	Emit(ctx context.Context, c Captured)
}

// Gateway relays requests to the configured upstream.
type Gateway struct {
	upstream string
	client   *http.Client
	maxBody  int64

	emitter   Emitter
	evaluator Evaluator
	gated     func(*http.Request) bool

	logf func(format string, args ...any)
}

// WithCapture turns on evidence emission. Observe-only: it changes what is
// reported, never what is forwarded.
func (g *Gateway) WithCapture(em Emitter) *Gateway {
	g.emitter = em
	return g
}

// WithGate turns on synchronous refusal. `gated` decides which calls get a
// verdict, and it is injected rather than decided here on purpose.
func (g *Gateway) WithGate(ev Evaluator, gated func(*http.Request) bool) *Gateway {
	g.evaluator = ev
	g.gated = gated
	return g
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
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				DialContext:         dialhook.Dial,
				ForceAttemptHTTP2:   true,
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
				DisableCompression:  true,
			},
		},
	}, nil
}

// ServeHTTP relays the request upstream.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Path only, never RequestURI: the query can carry content or a token.
	var start time.Time
	if g.verbose() {
		start = time.Now()
		g.vlog("→ %s %s", r.Method, r.URL.Path)
	}
	if !strings.HasPrefix(r.RequestURI, "/") {
		g.relayError(w, http.StatusBadRequest,
			"request target must be origin-form (a path beginning with /)")
		return
	}

	if strings.Contains(r.RequestURI, "#") {
		g.relayError(w, http.StatusBadRequest,
			"request target must not contain a fragment; it cannot be forwarded byte-identically")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, g.maxBody))
	if err != nil {
		var toolarge *http.MaxBytesError
		if errors.As(err, &toolarge) {
			g.relayError(w, http.StatusRequestEntityTooLarge, "request body exceeds the gateway relay limit")
			return
		}
		g.relayError(w, http.StatusBadRequest, "reading request body")
		return
	}

	var reqCapture RequestCapture
	capturing := g.emitter != nil || g.evaluator != nil
	if capturing {
		reqCapture = CaptureRequest(r.Method, g.upstream+r.RequestURI, r.Header,
			capturableBody(body, r.Header))
	}

	if g.evaluator != nil && g.gated != nil && g.gated(r) {
		decision := Decide(r.Context(), g.evaluator, true, reqCapture.ForGate())
		if !decision.Forward {
			WriteRefusal(w, decision)
			if g.emitter != nil {
				g.emitter.Emit(r.Context(), reqCapture.Complete(refusalStatus, nil, ""))
			}
			return
		}
	}

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
	outbound.ContentLength = int64(len(body))
	copyHeaders(outbound.Header, r.Header)

	resp, err := g.client.Do(outbound)
	if err != nil {
		if g.emitter != nil {
			g.emitter.Emit(r.Context(), reqCapture.Complete(0, nil, ""))
		}
		if errors.Is(err, context.Canceled) {
			return
		}
		g.relayError(w, http.StatusBadGateway, "upstream unreachable")
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	_ = http.NewResponseController(w).Flush()

	// Bounded by the same rune cap the wire has, so a long stream cannot grow
	// this without limit.
	var sink *captureSink
	if g.emitter != nil {
		sink = &captureSink{}
	}
	streamErr := g.streamTo(w, resp.Body, sink)

	if g.verbose() {
		outcome := "ok"
		if streamErr != nil {
			outcome = "stream aborted"
		}
		g.vlog("← %s %s %d in %s (%s)", r.Method, r.URL.Path, resp.StatusCode,
			time.Since(start).Round(time.Millisecond), outcome)
	}

	if g.emitter != nil {
		g.emitter.Emit(r.Context(), reqCapture.Complete(resp.StatusCode, resp.Header,
			capturableBody(sink.Bytes(), resp.Header)))
	}

	// After the emit, deliberately: the evidence of a failed call is exactly the
	// evidence an auditor needs, and panicking first would discard it.
	if streamErr != nil {
		panic(http.ErrAbortHandler)
	}
}

// capturableBody every guarantee capture.go's header comment makes about
// redaction evaporated silently, and the stored evidence was destroyed anyway.
func capturableBody(body []byte, h http.Header) string {
	if isContentEncoded(h) {
		return "[openbox: not captured; the body was content-encoded, so redaction could not inspect it]"
	}
	if len(body) > maxCaptureInputBytes {
		body = body[:maxCaptureInputBytes]
	}
	return string(body)
}

func isContentEncoded(h http.Header) bool {
	enc := strings.TrimSpace(h.Get("Content-Encoding"))
	return enc != "" && !strings.EqualFold(enc, "identity")
}

type captureSink struct {
	buf []byte
}

const maxCaptureSinkBytes = maxCaptureInputBytes

func (s *captureSink) Write(p []byte) {
	if s == nil || len(s.buf) >= maxCaptureSinkBytes {
		return
	}
	room := maxCaptureSinkBytes - len(s.buf)
	if len(p) > room {
		p = p[:room]
	}
	s.buf = append(s.buf, p...)
}

func (s *captureSink) String() string {
	return string(s.Bytes())
}

// Bytes returns the accumulated copy without a conversion, so a caller that
// only needs to inspect or bound it does not pay for a second copy of up to
// maxCaptureSinkBytes.
func (s *captureSink) Bytes() []byte {
	if s == nil {
		return nil
	}
	return s.buf
}

// streamTo the relay is unchanged by the tee: the write to the client happens
// first and its error is what ends the loop, so a capture problem can never
// abort a stream.
func (g *Gateway) streamTo(w http.ResponseWriter, src io.Reader, sink *captureSink) error {
	ctl := http.NewResponseController(w)
	defer func() { _ = ctl.SetWriteDeadline(time.Time{}) }()
	buf := make([]byte, relayBufferSize)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			_ = ctl.SetWriteDeadline(time.Now().Add(writeIdleTimeout))
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return nil
			}
			sink.Write(buf[:n])
			_ = ctl.Flush()
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

// relayError answers when the relay itself could not proceed.
func (g *Gateway) relayError(w http.ResponseWriter, status int, reason string) {
	// `reason` is this package's own fixed wording and never echoes request
	// detail (see the doc comment above).
	g.vlog("✗ relay refused: %d %s", status, reason)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"type":"error","error":{"type":"openbox_gateway_error","message":%q}}`, reason)
}

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
