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
	"net/http/httptrace"
	"net/textproto"
	"slices"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/gateway/internal/dialhook"
)

// hopByHopHeaders describe this connection rather than the message. Trailer is
// deliberately absent: it announces which fields arrive after the body, and
// dropping it wholesale is how trailers stopped propagating. Te stays, but its
// one end-to-end value is forwarded explicitly below.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Proxy-Connection":    true,
	"Te":                  true,
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

	capturesBody func(*http.Request) bool

	// attribution reads provider-specific correlation identifiers out of the
	// RAW request bytes. Injected as a closure because this package stays
	// provider-neutral: it carries the result, it does not know what the keys
	// mean. See WithRequestAttribution.
	attribution func(raw []byte) map[string]string

	logf func(format string, args ...any)
}

func (g *Gateway) now() time.Time { return time.Now() }

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

// WithBodyCapture narrows body capture to the classes that keep one; probes are
// ~40% of relayed POSTs and are scanned only to be dropped.
func (g *Gateway) WithBodyCapture(capturesBody func(*http.Request) bool) *Gateway {
	g.capturesBody = capturesBody
	return g
}

// WithRequestAttribution injects a provider-specific read of the RAW request
// bytes, run once per capturing call, independent of WithBodyCapture: the
// result is structural evidence and must not depend on whether this path
// keeps a body. The relay stays provider-neutral: it carries the result on
// RequestCapture.Attribution, it does not know what the keys mean.
func (g *Gateway) WithRequestAttribution(parse func(raw []byte) map[string]string) *Gateway {
	g.attribution = parse
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
	// Path only, never RequestURI: the query can carry content or a token. The clock
	// is unconditional: this is the relayed call's start.
	start := g.now()
	if g.verbose() {
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
	gatedCall := g.evaluator != nil && g.gated != nil && g.gated(r)
	keepBodies := gatedCall || g.capturesBody == nil || g.capturesBody(r)
	if capturing {
		captured := ""
		if keepBodies {
			captured = capturableRequestBody(body, r.Header)
		}
		reqCapture = CaptureRequest(r.Method, g.upstream+r.RequestURI, r.Header, captured, start)
		// Independent of keepBodies, deliberately: this reads the RAW bytes
		// already in hand, never the selected/bounded capture above, so it
		// keeps working under a posture that keeps no body at all.
		if g.attribution != nil {
			reqCapture.Attribution = g.attribution(body)
		}
	}

	if gatedCall {
		decision := Decide(r.Context(), g.evaluator, true, reqCapture.ForGate())
		if !decision.Forward {
			WriteRefusal(w, decision)
			if g.emitter != nil {
				g.emitter.Emit(r.Context(), reqCapture.Complete(refusalStatus, nil, "", g.now()))
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
	// The one Te value with end-to-end meaning. Without it the upstream is told
	// nothing downstream can take trailers, so it will not send any.
	if requestsTrailers(r.Header) {
		outbound.Header.Set("Te", "trailers")
	}
	outbound = outbound.WithContext(httptrace.WithClientTrace(outbound.Context(),
		&httptrace.ClientTrace{Got1xxResponse: forward1xx(w)}))

	resp, err := g.client.Do(outbound)
	if err != nil {
		if g.emitter != nil {
			g.emitter.Emit(r.Context(), reqCapture.Complete(0, nil, "", g.now()))
		}
		if errors.Is(err, context.Canceled) {
			return
		}
		g.relayError(w, http.StatusBadGateway, "upstream unreachable")
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	announceTrailers(w.Header(), resp.Trailer)
	w.WriteHeader(resp.StatusCode)

	_ = http.NewResponseController(w).Flush()

	// Bounded by the same rune cap the wire has, so a long stream cannot grow
	// this without limit.
	var sink *captureSink
	if g.emitter != nil && keepBodies {
		sink = &captureSink{}
	}
	streamErr := g.streamTo(w, resp.Body, sink)
	// End of stream is where the clock stops. Not at WriteHeader: that would report
	// time-to-first-byte as the call's latency.
	end := g.now()
	// resp.Trailer is only populated once the body has been read to EOF, which
	// is why this cannot happen with the header copy above.
	copyTrailers(w.Header(), resp.Trailer)

	if g.verbose() {
		outcome := "ok"
		if streamErr != nil {
			outcome = "stream aborted"
		}
		g.vlog("← %s %s %d in %s (%s)", r.Method, r.URL.Path, resp.StatusCode,
			end.Sub(start).Round(time.Millisecond), outcome)
	}

	if g.emitter != nil {
		respBody := ""
		if keepBodies {
			respBody = capturableBody(sink.Bytes(), resp.Header)
		}
		captured := reqCapture.Complete(resp.StatusCode, resp.Header, respBody, end)
		// Set on the returned value rather than threaded through Complete's own
		// signature: both are response-side facts Complete never had (it only
		// ever saw the already-computed respBody string), and capture_test.go
		// already calls Complete directly, so widening its signature would
		// touch a file this phase does not own for a fact this call site is
		// the only place that has.
		captured.ResponseBytesSeen = sink.Seen()
		captured.ResponseTruncated = responseTruncated(sink, captured.ResponseBody, streamErr)
		g.emitter.Emit(r.Context(), captured)
	}

	// After the emit, deliberately: the evidence of a failed call is exactly the
	// evidence an auditor needs, and panicking first would discard it.
	if streamErr != nil {
		panic(http.ErrAbortHandler)
	}
}

// capturableBody renders the body redaction will see. A content-encoded body is
// decoded from the teed copy only: forwarded bytes are never touched.
//
// This is the RESPONSE path. A reply starts at its beginning, so a head window is
// the right window and truncation is the right tool; see capturableRequestBody
// for why the request direction cannot use it.
func capturableBody(body []byte, h http.Header) string {
	if enc := contentEncoding(h); enc != "" {
		return decodeCapturable(body, enc)
	}
	if len(body) > maxCaptureInputBytes {
		// Marked, and with room for the mark inside the same bound: an unmarked cut
		// here is a clipped reply that reads as a finished one.
		return trimPartialRune(string(body[:maxCaptureInputBytes-len(bodyCutNote)])) + bodyCutNote
	}
	return string(body)
}

// capturableRequestBody is the request path, and it SELECTS where the response
// path truncates.
//
// It must see the whole body, which is why it does not reuse capturableBody: that
// function's 256 KiB head cut lands inside the message array of a p50 520 KB
// request, so selecting afterwards would only ever be choosing among the OLDEST
// turns. Bounding is the selector's own job instead, and its budget is smaller
// than this one.
func capturableRequestBody(body []byte, h http.Header) string {
	if enc := contentEncoding(h); enc != "" {
		// A compressed request is unobserved in the corpus, and decodeCapturable's
		// bound applies before selection can see the plaintext -- so a large
		// compressed body arrives already head-cut and falls back to a marked
		// window, which is the honest outcome rather than a silent one.
		return selectModelCallRequest(decodeCapturable(body, enc))
	}
	return selectModelCallRequest(string(body))
}

// contentEncoding joins every Content-Encoding LINE, not just the first.
//
// `Get` returns element [0], and isToken refuses only a list that arrived
// comma-joined in one value -- so two header lines, the same list semantically,
// reduced to their first token. A br(gzip(json)) reply announced that way
// gunzipped cleanly and returned raw brotli, which the keyword-driven redactor
// cannot see into, stored as the decoded reply. Joining hands the pair to the
// two-layer refusal newDecompressor already had.
func contentEncoding(h http.Header) string {
	enc := strings.TrimSpace(strings.Join(h.Values("Content-Encoding"), ","))
	if enc == "" || strings.EqualFold(enc, "identity") {
		return ""
	}
	return enc
}

type captureSink struct {
	buf []byte
	// seen is every byte OFFERED to Write, including whatever the bound and
	// clamp below then dropped. It is the only honest pre-cut count: buf itself
	// stops at exactly maxCaptureSinkBytes and can never say more than that
	// about itself. Incremented before the bound check, deliberately -- a
	// clamped or wholly-dropped chunk still arrived.
	seen int
	// incomplete is set when the relay gave up copying response bytes before
	// the upstream body was exhausted, for a reason Cut cannot see: the
	// downstream write to the CLIENT failed (streamTo's write-failure branch),
	// not the sink's own bound. seen and len(buf) agree exactly in that case --
	// the chunk that failed to write was never offered to Write at all -- so
	// nothing about the sink's own byte bookkeeping can detect it. streamTo
	// sets this directly at the one place that knows.
	incomplete bool
}

// MarkIncomplete records that the relay is abandoning this stream before EOF
// for a reason Cut's own byte bookkeeping cannot see. Nil-safe, like Write.
func (s *captureSink) MarkIncomplete() {
	if s == nil {
		return
	}
	s.incomplete = true
}

// Incomplete reports whether MarkIncomplete was called. Nil-safe, like Cut.
func (s *captureSink) Incomplete() bool {
	return s != nil && s.incomplete
}

const maxCaptureSinkBytes = maxCaptureInputBytes

func (s *captureSink) Write(p []byte) {
	if s == nil {
		return
	}
	s.seen += len(p)
	if len(s.buf) >= maxCaptureSinkBytes {
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

// Seen is the total bytes this sink was ever offered, nil-safe like Bytes.
// It can exceed len(Bytes()) -- that gap IS the cut -- and rides the wire as
// Captured/Span.ResponseBytesSeen. It counts bytes as they arrive OFF THE
// WIRE, still compressed when the response was: internal/client's
// original_bytes note deliberately does NOT source from this value (a
// compressed count is not comparable to the decoded body it would be
// claiming to bound), so treat it as evidence about the TRANSPORT, not as a
// decoded-content byte count.
func (s *captureSink) Seen() int {
	if s == nil {
		return 0
	}
	return s.seen
}

// Cut reports that this sink dropped bytes past its bound. It is the only
// component that can know: Write clamps buf to exactly maxCaptureSinkBytes, the
// same number capturableBody compares against with `>`, so buf's own length can
// never exceed it and that comparison can never fire through this path. seen
// has no such ceiling, so seen > len(buf) is the one comparison that can.
func (s *captureSink) Cut() bool {
	return s != nil && s.seen > len(s.buf)
}

// responseTruncated is true when the emitted ResponseBody is not the whole
// reply. Four independent things can make that so, and each is a distinct
// input because none of the others can observe it:
//   - streamErr != nil: streamTo's own READ from upstream failed before EOF
//     (the connection to the PROVIDER broke). This is g.streamTo's return
//     value, computed at the call site and otherwise consulted only to decide
//     whether to panic(http.ErrAbortHandler) -- it must ALSO reach this note,
//     or the exact call an auditor most needs marked complete-or-not reports
//     complete by default.
//   - sink.Incomplete(): streamTo's own WRITE to the CLIENT failed (the
//     developer's tool disconnected). streamTo deliberately returns nil for
//     this -- turning it into an error would incorrectly trip the panic path
//     above -- so streamErr is nil here and cannot report it; the sink carries
//     it instead, set at the one place that knows (see MarkIncomplete).
//   - sink.Cut(): the sink dropped bytes past its own bound.
//   - stored ends in bodyCutNote: noteCut (capture.go, the capRunes path) and
//     decodeCapturable (decode.go, the compressed-body path) each append
//     independently of the sink, and can fire without it ever reaching its
//     bound. `stored` must be the value AFTER Complete (RequestCapture.Complete),
//     because capRunes' own cut happens inside captureBody, which Complete
//     calls; checking the pre-Complete value would miss that half of the OR.
func responseTruncated(sink *captureSink, stored string, streamErr error) bool {
	return streamErr != nil || sink.Incomplete() || sink.Cut() || strings.HasSuffix(stored, bodyCutNote)
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
				// The client is gone: relaying is done trying, and this chunk
				// was never offered to sink either, so seen and len(buf) will
				// agree and Cut alone would miss the cut. Mark it as data on
				// the sink -- not by returning writeErr, which would flip the
				// panic(http.ErrAbortHandler) path below into firing for a
				// client that already left.
				sink.MarkIncomplete()
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

// requestsTrailers reports whether the client said it can take trailers.
func requestsTrailers(h http.Header) bool {
	for _, value := range h["Te"] {
		for part := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "trailers") {
				return true
			}
		}
	}
	return false
}

// announceTrailers names the trailer fields before the body, which is what lets
// a client know to wait for them.
func announceTrailers(dst http.Header, trailer http.Header) {
	if len(trailer) == 0 {
		return
	}
	names := make([]string, 0, len(trailer))
	for name := range trailer {
		names = append(names, name)
	}
	slices.Sort(names) // stable across runs; the set is unordered
	dst.Set("Trailer", strings.Join(names, ", "))
}

// copyTrailers publishes the upstream's trailers. TrailerPrefix also carries a
// trailer that was never announced, which is legal and would otherwise drop.
func copyTrailers(dst http.Header, trailer http.Header) {
	for name, values := range trailer {
		dst[http.TrailerPrefix+name] = append([]string(nil), values...)
	}
}

// forward1xx relays an informational response instead of swallowing it: a
// 100-continue or 103 early-hints the provider sent otherwise never happened.
func forward1xx(w http.ResponseWriter) func(int, textproto.MIMEHeader) error {
	return func(code int, header textproto.MIMEHeader) error {
		h := w.Header()
		for name, values := range header {
			h[name] = append([]string(nil), values...)
		}
		w.WriteHeader(code)
		// The 1xx headers are that response's, not the final one's.
		for name := range header {
			h.Del(name)
		}
		return nil
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
