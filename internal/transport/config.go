package transport

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"time"
)

// DefaultInterceptHost is the one host this lane terminates TLS for. Single-
// host interception is the bound that makes that decision's reversal
// defensible (OD2): every other CONNECT is blind-tunnelled, never decrypted.
const DefaultInterceptHost = "api.anthropic.com"

// DefaultAddr is this lane's deterministic loopback listen address. Three
// loopback daemons can be installed on one machine, and two sharing a port
// means whichever starts second cannot bind; which under launchd's KeepAlive
// is a restart loop rather than an error anyone sees.
const DefaultAddr = "127.0.0.1:8790"

const resolveTimeout = 2 * time.Second

// Config is what the transport lane needs to run.
type Config struct {
	// Addr is the proxy's listen address. It must resolve to loopback: this proxy
	// performs no caller authentication AND terminates TLS for the provider's
	// hostname, so a non-loopback listener would let anything on the network
	// route its model calls through this machine's CA.
	Addr string

	// Allowlist names the hosts that are TLS-terminated. Empty means the default
	// single host; an explicitly configured set is kept as-is and never widened
	// by Validate.
	Allowlist Allowlist

	// Upstream overrides where an intercepted request is forwarded. Empty is the
	// production value and means "derive it from the CONNECT host" (UpstreamFor),
	// which is the only correct rule for a relay that must not retarget a call.
	Upstream string
}

// Validate fills defaults and rejects a configuration that would break the two
// invariants this lane rests on: loopback-only binding, and interception
// bounded to an allowlist.
func (c *Config) Validate() error {
	if c.Addr == "" {
		c.Addr = DefaultAddr
	}
	if len(c.Allowlist.hosts) == 0 {
		c.Allowlist = NewAllowlist(DefaultInterceptHost)
	}

	if c.Upstream != "" {
		u, err := url.Parse(c.Upstream)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("transport: upstream %q must be an absolute URL", c.Upstream)
		}
	}

	host, port, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return fmt.Errorf("transport: listen address %q is not host:port: %w", c.Addr, err)
	}
	if port == "" {
		return fmt.Errorf("transport: listen address %q names no port", c.Addr)
	}
	return requireLoopback(host)
}

// UpstreamFor is the absolute base URL an intercepted CONNECT target forwards
// to. The port is carried when it is not 443, because dropping it would
// silently retarget the call to the default port.
func UpstreamFor(connectTarget string) string {
	host, port, err := net.SplitHostPort(connectTarget)
	if err != nil {
		host, port = connectTarget, ""
	}
	host = normalizeHost(host)
	if port == "" || port == "443" {
		return "https://" + host
	}
	return "https://" + net.JoinHostPort(host, port)
}

func requireLoopback(host string) error {
	if host == "" || host == "0.0.0.0" || host == "::" || host == "*" {
		return fmt.Errorf("transport: listen host %q binds every interface; loopback is required", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("transport: listen host %q is not loopback", host)
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return fmt.Errorf("transport: listen host %q is not an IP and does not resolve "+
			"(loopback is required): %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("transport: listen host %q resolved to nothing; loopback is required", host)
	}
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("transport: listen host %q resolves to %s, which is not loopback", host, addr)
		}
	}
	return nil
}

// ProxyURL is the value the proxy environment keys are set to (phase 12 owns
// the activation itself; this is the one place that spells the URL).
func (c Config) ProxyURL() string {
	u := url.URL{Scheme: "http", Host: c.Addr}
	return u.String()
}
