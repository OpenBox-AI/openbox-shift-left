package gateway

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// DefaultAddr is the deterministic loopback listen address. The port is fixed
// rather than scanned: a developer's tooling, the doctor check and the managed
// config all have to agree on one value without discovering it.
const DefaultAddr = "127.0.0.1:8788"

// DefaultUpstream is the provider the gateway substitutes for.
const DefaultUpstream = "https://api.anthropic.com"

// resolveTimeout bounds the one network call configuration validation makes.
const resolveTimeout = 2 * time.Second

// Config is the gateway's listen and upstream configuration.
type Config struct {
	// Addr is the listen address. It must resolve to loopback: the gateway
	// performs no caller authentication, so loopback binding IS the caller
	// boundary. A non-loopback listener would relay arbitrary traffic upstream
	// under whatever credential a remote caller presented.
	Addr string

	// Upstream is the provider base URL every request is forwarded to. It is
	// address-configurable so the same binary can run somewhere other than the
	// developer's machine later, without a code change.
	Upstream string
}

// Validate fills defaults and rejects a configuration that would break either
// invariant the gateway rests on: loopback-only binding and an absolute
// upstream base URL.
func (c *Config) Validate() error {
	if c.Addr == "" {
		c.Addr = DefaultAddr
	}
	if c.Upstream == "" {
		c.Upstream = DefaultUpstream
	}

	host, _, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return fmt.Errorf("gateway: listen address %q is not host:port: %w", c.Addr, err)
	}
	if err := requireLoopback(host); err != nil {
		return err
	}

	u, err := url.Parse(c.Upstream)
	if err != nil {
		return fmt.Errorf("gateway: upstream %q is not a URL: %w", c.Upstream, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("gateway: upstream %q must be an absolute URL", c.Upstream)
	}
	// TrimRight, not TrimSuffix: one call strips one slash, so "…com//" kept a
	// slash and every request URL grew a doubled separator.
	c.Upstream = strings.TrimRight(c.Upstream, "/")

	// A relay pointed at its own listener forwards every request back into
	// itself, so one call spawns another until goroutines or sockets run out.
	// Fail at startup instead, where the mistake is legible.
	if selfReferential(c.Addr, u) {
		return fmt.Errorf("gateway: upstream %q is this gateway's own listen address; requests would loop", c.Upstream)
	}
	return nil
}

// selfReferential reports whether the upstream names the listener.
//
// A raw string comparison misses the realistic version of this mistake -- the
// same loopback socket spelled two ways, "localhost:8788" against
// "127.0.0.1:8788" -- which is the exact class requireLoopback resolves rather
// than trusts. So the port is compared numerically and loopback spellings are
// treated as equivalent.
//
// Scope, deliberately: only a loopback-shaped upstream is considered. A remote
// host cannot be this listener, and resolving one here would put a DNS call on
// every startup for the default configuration.
func selfReferential(listenAddr string, upstream *url.URL) bool {
	listenHost, listenPort, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return false
	}
	upstreamHost := upstream.Hostname()
	upstreamPort := upstream.Port()
	if upstreamPort == "" {
		switch upstream.Scheme {
		case "https":
			upstreamPort = "443"
		case "http":
			upstreamPort = "80"
		default:
			return false
		}
	}
	if upstreamPort != listenPort {
		return false
	}
	return isLoopbackSpelling(listenHost) && isLoopbackSpelling(upstreamHost)
}

// isLoopbackSpelling covers the spellings that name the local machine without a
// resolver round-trip.
func isLoopbackSpelling(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// requireLoopback rejects any bind host that is not loopback. An empty host
// (":8788") and the wildcards bind every interface, so they are refused by name
// rather than left to the IP check — ParseIP would reject most of them anyway,
// but naming them produces an error that says which mistake was made.
//
// A NAME is resolved rather than trusted. "localhost" is only loopback because
// the resolver says so, and hosts files, DNS, nsswitch and interception agents
// can all say otherwise; accepting the string on faith would let a resolver
// decision silently move the listener off loopback. Every returned address has
// to be loopback, not just the first.
func requireLoopback(host string) error {
	if host == "" || host == "0.0.0.0" || host == "::" || host == "*" {
		return fmt.Errorf("gateway: listen host %q binds every interface; loopback is required", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("gateway: listen host %q is not loopback", host)
		}
		return nil
	}

	// Bounded: DefaultResolver with a background context waits as long as the OS
	// resolver does, which on a slow or unreachable DNS path stalls startup with
	// no output.
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return fmt.Errorf("gateway: listen host %q is not an IP and does not resolve: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("gateway: listen host %q resolved to nothing", host)
	}
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("gateway: listen host %q resolves to %s, which is not loopback", host, addr)
		}
	}
	return nil
}

// Listen binds the configured address and re-checks the result.
//
// Validate happens before a bind and can only inspect a string; this checks what
// the kernel actually handed back. That closes the window between the two — a
// resolver answering differently at bind time than at validate time, or a
// dual-stack surprise — and it is the check that cannot be fooled by a name,
// because at this point there is no name left, only an address.
// It returns the validated config so a caller does not have to validate again --
// which also means a hostname is resolved once per start, not once per call site.
func Listen(cfg Config) (net.Listener, Config, error) {
	if err := cfg.Validate(); err != nil {
		return nil, cfg, err
	}
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, cfg, fmt.Errorf("gateway: cannot listen on %s: %w", cfg.Addr, err)
	}
	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		listener.Close()
		return nil, cfg, fmt.Errorf("gateway: listener bound a %T, not a TCP address", listener.Addr())
	}
	if !tcpAddr.IP.IsLoopback() {
		listener.Close()
		return nil, cfg, fmt.Errorf("gateway: listener bound %s, which is not loopback", tcpAddr.IP)
	}
	return listener, cfg, nil
}
