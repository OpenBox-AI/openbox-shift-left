package gateway

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// DefaultAddr is the deterministic loopback listen address.
const DefaultAddr = "127.0.0.1:8788"

// DefaultUpstream is the provider the gateway substitutes for.
const DefaultUpstream = "https://api.anthropic.com"

const resolveTimeout = 2 * time.Second

// Config is the gateway's listen and upstream configuration.
type Config struct {
	// Addr is the listen address. It must resolve to loopback: the gateway
	// performs no caller authentication, so loopback binding IS the caller
	// boundary.
	Addr string

	// Upstream is the provider base URL every request is forwarded to.
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
	c.Upstream = strings.TrimRight(c.Upstream, "/")

	if selfReferential(c.Addr, u) {
		return fmt.Errorf("gateway: upstream %q is this gateway's own listen address; requests would loop", c.Upstream)
	}
	return nil
}

// selfReferential scope, deliberately: only a loopback-shaped upstream is
// considered.
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

func isLoopbackSpelling(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

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
