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
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
)

// connectTimeout bounds the TLS handshake on an intercepted tunnel. A client
// that completed CONNECT and then never sent a ClientHello is stuck, not slow,
// and an unbounded handshake holds the goroutine and the connection forever.
const connectTimeout = 30 * time.Second

// readHeaderTimeout bounds how long an idle tunnel may hold a goroutine before
// sending a request line.
const readHeaderTimeout = 30 * time.Second

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

	// handlerFor builds the relay that serves one intercepted host. Production
	// must never get a stub, so TestProductionHandlerIsTheGatewayRelay asserts
	// the default factory returns a real *gateway.Gateway.
	handlerFor func(host string) (http.Handler, error)

	mu       sync.Mutex
	handlers map[string]http.Handler
}

// New builds the relay. Taking it here means there is no reachable state in
// which this lane relays without recording. It also clears the inherited proxy
// environment; a process-wide side effect, deliberately, see
// clearInheritedProxyEnv.
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
	return p.Apply(opts...), nil
}

// Option configures a Proxy.
type Option func(*Proxy)

// WithVerbose turns on per-connection commentary.
func WithVerbose(logf func(format string, args ...any)) Option {
	return func(p *Proxy) { p.logf = logf }
}

// Apply configures a Proxy after construction.
func (p *Proxy) Apply(opts ...Option) *Proxy {
	for _, o := range opts {
		o(p)
	}
	return p
}

// ClearedProxyEnv names the environment variables New removed.
func (p *Proxy) ClearedProxyEnv() []string { return p.clearedEnv }

// Addr is the configured listen address.
func (p *Proxy) Addr() string { return p.cfg.Addr }

// Hosts names what this relay intercepts, for the startup line and doctor.
func (p *Proxy) Hosts() []string { return p.cfg.Allowlist.Hosts() }

// ServeHTTP makes the Proxy an http.Handler, so the CLI serves it exactly like
// the gateway.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.engine.ServeHTTP(w, r)
}

func (p *Proxy) intercepts(host string) bool { return p.cfg.Allowlist.Allows(host) }

// onConnect decides what happens to one CONNECT. Everything else is accepted,
// which in goproxy means a blind tunnel: bytes copied both ways, never
// decrypted, never inspected, never captured.
func (p *Proxy) onConnect(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
	if !p.intercepts(host) {
		p.vlog("tunnel %s (not allowlisted; no interception, no capture)", host)
		return &goproxy.ConnectAction{Action: goproxy.ConnectAccept}, host
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

// ServeIntercepted terminates TLS on an already-accepted CONNECT and serves
// the relay over it.
func (p *Proxy) ServeIntercepted(client net.Conn, host string) error {
	return p.interceptConn(client, host)
}

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
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed || state == http.StateHijacked {
				listener.Close()
			}
		},
	}
	err = srv.Serve(listener)
	if errors.Is(err, errListenerDrained) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

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

// newRelay is the production handler factory: the existing gateway relay,
// aimed at the intercepted host, with capture wired.
func (p *Proxy) newRelay(host string) (http.Handler, error) {
	upstream := p.cfg.Upstream
	if upstream == "" {
		upstream = UpstreamFor(host)
	}
	g, err := gateway.New(gateway.Config{
		Addr:     "127.0.0.1:0",
		Upstream: upstream,
	})
	if err != nil {
		return nil, fmt.Errorf("transport: build relay for %s: %w", host, err)
	}
	g = g.WithCapture(p.emitter)
	if p.logf != nil {
		g = g.WithVerbose(p.logf)
	}
	return g, nil
}

func (p *Proxy) vlog(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}

// clearInheritedProxyEnv removes the proxy variables from this process.
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

var errListenerDrained = errors.New("transport: connection served")

type singleConnListener struct {
	conn      net.Conn
	once      sync.Once
	closeOnce sync.Once
	done      chan struct{}
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
	<-l.done
	return nil, errListenerDrained
}

func (l *singleConnListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return nil
}

func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
