package transport

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/openbox-ai/openbox-shift-left/gateway"
)

// proxy.go — the in-path model-call lane (`:proxy:`, ADR-0022).
//
// THE SHAPE, and why it is not the one phase 11 sketched. The phase proposed
// goproxy's MITM action plus a hand-built "streaming tee" into gateway's capture
// sink. This does the opposite: goproxy HIJACKS the allowlisted CONNECT, we
// terminate TLS ourselves, and we serve the EXISTING gateway.Gateway over the
// resulting connection.
//
// The reason is the rule this repo puts first. gateway.ServeHTTP already holds
// byte-identical forwarding, per-chunk SSE streaming, the fingerprint-before-
// redact capture ordering and the 64KB cap — proven by 81 tests including a full
// identity suite. A tee would be a SECOND implementation of all of it, on the
// enforcement path, which is precisely the copy-paste drift CLAUDE.md names as
// the original sin. It would also have run through goproxy's MITM response copy,
// which the spike explicitly did NOT measure.
//
// So the division is: goproxy owns CONNECT parsing, the blind tunnel and the
// plain-HTTP forward; we own the allowlist, the CA, and the ~30 lines that turn
// one hijacked connection into an http.Server. gateway owns everything that
// touches bytes or evidence.
//
// What that leaves genuinely new and therefore genuinely unproven: CONNECT, the
// TLS termination, and the CA. Nothing else in this file touches a request body.

// connectTimeout bounds the TLS handshake on an intercepted tunnel. A client that
// completed CONNECT and then never sent a ClientHello is stuck, not slow, and an
// unbounded handshake holds the goroutine and the connection forever.
const connectTimeout = 30 * time.Second

// readHeaderTimeout bounds how long an idle tunnel may hold a goroutine before
// sending a request line.
//
// There is deliberately NO ReadTimeout or WriteTimeout on the server below: a
// streamed completion legitimately runs for minutes, and either would abort it
// mid-stream. Same reasoning, and the same omission, as the gateway's own server.
const readHeaderTimeout = 30 * time.Second

// proxyEnvKeys are the environment variables that would make this daemon's own
// upstream leg dial itself. See clearInheritedProxyEnv.
var proxyEnvKeys = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "all_proxy",
}

// Proxy is the transport lane's relay.
type Proxy struct {
	cfg     Config
	ca      *CA
	emitter gateway.Emitter
	engine  *goproxy.ProxyHttpServer
	logf    func(format string, args ...any)

	clearedEnv []string

	// handlerFor builds the relay that serves one intercepted host. A FIELD, so a
	// test can rehearse the CONNECT/TLS choreography over an in-memory pipe with
	// no upstream at all — this host denies bind, and a socket-only test would
	// SKIP and leave the choreography unexercised.
	//
	// Production must never get a stub, so TestProductionHandlerIsTheGatewayRelay
	// asserts the default factory returns a real *gateway.Gateway. That is the
	// control for the failure shape CLAUDE.md names: a fake at each end of a seam
	// with no implementation between them keeps both suites green.
	handlerFor func(host string) (http.Handler, error)

	mu       sync.Mutex
	handlers map[string]http.Handler
}

// New builds the relay.
//
// The emitter is REQUIRED, and that is this constructor's most important
// property. The gateway lane shipped with a nil emitter because nothing in the
// binary called WithCapture: the relay worked perfectly and discarded every
// capture, for as long as nobody looked. Taking it here means there is no
// reachable state in which this lane relays without recording.
//
// It also CLEARS the inherited proxy environment — a process-wide side effect,
// deliberately, see clearInheritedProxyEnv.
func New(cfg Config, ca *CA, emitter gateway.Emitter, opts ...Option) (*Proxy, error) {
	if ca == nil {
		return nil, errors.New("transport: no CA; nothing can be intercepted without one")
	}
	if emitter == nil {
		return nil, errors.New("transport: no emitter. A relay that cannot record is the gap this " +
			"lane exists to close, so it is refused at construction rather than discovered in the field")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	p := &Proxy{
		cfg:        cfg,
		ca:         ca,
		emitter:    emitter,
		engine:     NewIdentityProxy(),
		clearedEnv: clearInheritedProxyEnv(),
		handlers:   map[string]http.Handler{},
	}
	p.handlerFor = p.newRelay
	p.engine.OnRequest().HandleConnectFunc(p.onConnect)
	return p, nil
}

// Option configures a Proxy.
type Option func(*Proxy)

// WithVerbose turns on per-connection commentary. Nil-safe; silent by default.
func WithVerbose(logf func(format string, args ...any)) Option {
	return func(p *Proxy) { p.logf = logf }
}

// Apply is how the CLI passes options after construction validated.
func (p *Proxy) Apply(opts ...Option) *Proxy {
	for _, o := range opts {
		o(p)
	}
	return p
}

// ClearedProxyEnv names the environment variables New removed.
//
// Reported rather than silent: a daemon that quietly dropped the developer's
// proxy configuration would be its own mystery to diagnose.
func (p *Proxy) ClearedProxyEnv() []string { return p.clearedEnv }

// Addr is the configured listen address.
func (p *Proxy) Addr() string { return p.cfg.Addr }

// Hosts names what this relay intercepts, for the startup line and doctor.
func (p *Proxy) Hosts() []string { return p.cfg.Allowlist.Hosts() }

// ServeHTTP makes the Proxy an http.Handler, so the CLI serves it exactly like
// the gateway. Plain (non-CONNECT) requests take goproxy's own forward path with
// the spike's byte-identity settings and are NOT captured — the provider does not
// serve model calls over cleartext, so a plaintext request here is a probe or a
// misconfiguration, not evidence.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.engine.ServeHTTP(w, r)
}

// intercepts reports whether a CONNECT target is TLS-terminated.
func (p *Proxy) intercepts(host string) bool { return p.cfg.Allowlist.Allows(host) }

// onConnect decides what happens to one CONNECT.
//
// Two outcomes and no third. An allowlisted host is HIJACKED — we take the raw
// connection and terminate TLS on it. Everything else is ACCEPTED, which in
// goproxy means a blind tunnel: bytes copied both ways, never decrypted, never
// inspected, never captured. That bound is what makes intercepting anything
// defensible at all.
func (p *Proxy) onConnect(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
	if !p.intercepts(host) {
		p.vlog("tunnel %s (not allowlisted; no interception, no capture)", host)
		return goproxy.OkConnect, host
	}
	return &goproxy.ConnectAction{
		Action: goproxy.ConnectHijack,
		Hijack: func(_ *http.Request, client net.Conn, _ *goproxy.ProxyCtx) {
			if err := p.interceptConn(client, host); err != nil {
				p.vlog("intercept %s ended: %v", host, err)
			}
		},
	}, host
}

// interceptConn terminates TLS on a hijacked CONNECT and serves the relay over it.
//
// goproxy writes NO response for ConnectHijack (https.go, the ConnectHijack
// case) — the 200 below is ours to send, and the client will not start its TLS
// handshake until it arrives.
func (p *Proxy) interceptConn(client net.Conn, host string) error {
	defer client.Close()

	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return fmt.Errorf("transport: write CONNECT response for %s: %w", host, err)
	}

	cfg, err := p.ca.ServerConfigFor(host)
	if err != nil {
		return err
	}
	tlsConn := tls.Server(client, cfg)
	// Bounded: a client that completed CONNECT and never sent a ClientHello would
	// otherwise hold this goroutine forever. Cleared afterwards, because the same
	// deadline on a streamed completion would abort it mid-response.
	if err := tlsConn.SetDeadline(time.Now().Add(connectTimeout)); err != nil {
		return fmt.Errorf("transport: set handshake deadline for %s: %w", host, err)
	}
	if err := tlsConn.Handshake(); err != nil {
		return fmt.Errorf("transport: TLS handshake for %s: %w", host, err)
	}
	if err := tlsConn.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("transport: clear handshake deadline for %s: %w", host, err)
	}
	if proto := tlsConn.ConnectionState().NegotiatedProtocol; proto == "h2" {
		// Unreachable while ServerConfigFor advertises only http/1.1, and checked
		// anyway: the relay behind this speaks HTTP/1.1, so an h2 connection would
		// fail to parse every request and the developer would see a broken tool
		// with no mention of OpenBox anywhere.
		return fmt.Errorf("transport: %s negotiated h2, which this relay does not speak", host)
	}

	handler, err := p.relayFor(host)
	if err != nil {
		return err
	}

	p.vlog("intercept %s (TLS terminated, relaying)", host)
	listener := newSingleConnListener(tlsConn)
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		// No ReadTimeout or WriteTimeout: see readHeaderTimeout.
		//
		// ConnState is what ENDS this server. Serve loops on Accept, and the
		// listener below has exactly one connection to give — so once that
		// connection closes, nothing would ever return from the second Accept and
		// this goroutine would live as long as the daemon. One leaked goroutine and
		// one leaked fd per model-call tunnel is not a slow leak; Claude Code opens
		// tunnels continuously.
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed || state == http.StateHijacked {
				listener.Close()
			}
		},
	}
	// One connection, one server. http.Serve owns the keep-alive loop, so a tunnel
	// carrying many requests — which is the ordinary case, Claude Code reuses
	// connections heavily — is handled by net/http rather than by a hand-written
	// loop here.
	err = srv.Serve(listener)
	if errors.Is(err, errListenerDrained) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// relayFor returns the cached relay for one intercepted host.
func (p *Proxy) relayFor(host string) (http.Handler, error) {
	key := normalizeHost(host)
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.handlers[key]; ok {
		return h, nil
	}
	h, err := p.handlerFor(host)
	if err != nil {
		return nil, err
	}
	p.handlers[key] = h
	return h, nil
}

// newRelay is the production handler factory: the existing gateway relay, aimed
// at the intercepted host, with capture wired.
//
// The Addr here is never bound. gateway.Config.Validate requires a loopback
// listen address because the gateway's own listener IS its caller boundary; this
// relay is reached only over a connection that already passed through the
// allowlist, so the field is satisfied rather than used. Port 0 says so.
func (p *Proxy) newRelay(host string) (http.Handler, error) {
	g, err := gateway.New(gateway.Config{
		Addr:     "127.0.0.1:0",
		Upstream: UpstreamFor(host),
	})
	if err != nil {
		return nil, fmt.Errorf("transport: build relay for %s: %w", host, err)
	}
	g = g.WithCapture(p.emitter)
	if p.logf != nil {
		g = g.WithVerbose(p.logf)
	}
	// Note what is NOT called: WithGate. Refusal stays dormant until probe A names
	// a shape Claude Code does not retry around, and TestTheGateIsNotWired holds
	// it — structurally, by there being no evaluator, rather than by asserting a
	// wired one stays quiet.
	return g, nil
}

func (p *Proxy) vlog(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}

// clearInheritedProxyEnv removes the proxy variables from THIS process.
//
// Phase 12 activates this lane by putting HTTPS_PROXY=http://127.0.0.1:<port>
// into the CLIENT's environment. If the daemon's own environment inherits it —
// a launchd setenv, /etc/environment, a login-shell export — then this relay's
// upstream leg dials the proxy, which is this process, and every intercepted call
// recurses until sockets run out. Both legs read the environment: gateway.New
// sets Proxy: http.ProxyFromEnvironment on its client, and NewIdentityProxy sets
// the same on goproxy's transport, so one clear fixes both.
//
// ORDERING MATTERS AND IS WHY THIS LIVES IN New. net/http caches the environment
// behind a sync.Once the first time ProxyFromEnvironment is consulted, so
// clearing after the first outbound request has no effect. Doing it in the
// constructor puts it before any request this daemon can make. It is a
// process-wide side effect in a constructor, which is normally wrong; here the
// alternative is a rule the caller must remember, and the failure it prevents is
// total.
//
// Consequence to own: this lane does not chain through a corporate proxy. That is
// the right default for a beta (OD3) — direct upstream is what the gateway lane
// already does — and chaining, if an org needs it, is an explicit phase-12
// re-injection rather than something inherited by accident.
func clearInheritedProxyEnv() []string {
	var cleared []string
	for _, k := range proxyEnvKeys {
		if os.Getenv(k) == "" {
			continue
		}
		if err := os.Unsetenv(k); err == nil {
			cleared = append(cleared, k)
		}
	}
	return cleared
}

// errListenerDrained ends the single-connection server cleanly.
var errListenerDrained = errors.New("transport: connection served")

// singleConnListener hands http.Server exactly one connection.
//
// The whole reason this type exists is to reuse net/http's connection loop —
// keep-alive, request parsing, response framing, context cancellation on close —
// for a connection that arrived over CONNECT instead of over Accept. Writing that
// loop by hand is where a relay quietly stops supporting a second request on the
// same tunnel.
type singleConnListener struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
}

func newSingleConnListener(c net.Conn) *singleConnListener {
	return &singleConnListener{conn: c, done: make(chan struct{})}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	// The second Accept must not return an error immediately and forever in a
	// tight loop: http.Server treats a non-temporary error as fatal and returns,
	// which is what we want, but it logs nothing useful if it spins. Blocking
	// until Close, then reporting a sentinel, ends Serve exactly once.
	<-l.done
	return nil, errListenerDrained
}

func (l *singleConnListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
