package transport

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"time"
)

// DefaultInterceptHost is claude-code's exact-match row in the host table
// (hosttable.go), kept as a named value for callers built before the
// per-provider table existed. It is no longer the only host this lane
// terminates TLS for: an installed provider's whole table union is, and the
// CA itself carries no host bound at all.
var DefaultInterceptHost = exactHostFor("claude-code")

// DefaultAddr is this lane's deterministic loopback listen address. Three
// loopback daemons can be installed on one machine, and two sharing a port
// means whichever starts second cannot bind; which under launchd's KeepAlive
// is a restart loop rather than an error anyone sees.
const DefaultAddr = "127.0.0.1:8790"

const resolveTimeout = 2 * time.Second

// Config is what the transport lane needs to run.
type Config struct {
	// Addr is the proxy's listen address. It must resolve to loopback: this proxy
	// performs no caller authentication AND terminates TLS for every host in
	// the installed providers' union, so a non-loopback listener would let
	// anything on the network route its model calls through this machine's CA.
	Addr string

	// Allowlist names the hosts that are TLS-terminated. Zero value means the
	// default derived from Providers; an explicitly configured set (including
	// one built from a zero-length Providers, see below) is kept as-is and
	// never widened by Validate.
	Allowlist Allowlist

	// Providers names the installed providers whose host-table union this lane
	// intercepts and PACs. Nil (the zero value) defaults to
	// {"claude-code"}, so today's behaviour -- an unconfigured daemon
	// intercepting api.anthropic.com, now plus the claude.ai suffix the table
	// also carries -- is what a caller that never sets this field gets. An
	// explicitly EMPTY, non-nil slice is a deliberately different value: it
	// means every provider was uninstalled, so Validate must NOT fall back to
	// claude-code -- the union, the allowlist and the PAC all become empty
	// (intercept nothing, PAC returns DIRECT for every host), never the
	// default host. Wiring this from the activation record is the caller's
	// job; this package only defaults and consumes it.
	Providers []string

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
	// Only a nil Providers (never set) gets the default; an explicitly empty,
	// non-nil slice means every provider was uninstalled and must stay empty.
	// A caller that supplies its own Allowlist but no Providers gets an empty
	// provider set rather than the default: the PAC is generated from
	// Providers, so defaulting it would route hosts the caller's allowlist
	// never named. An all-DIRECT PAC is the safe mismatch.
	explicitAllowlist := len(c.Allowlist.hosts) > 0 || len(c.Allowlist.suffixes) > 0
	if c.Providers == nil {
		if explicitAllowlist {
			c.Providers = []string{}
		} else {
			c.Providers = []string{"claude-code"}
		}
	}
	if !explicitAllowlist {
		c.Allowlist = AllowlistFor(c.Providers...)
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

// ProxyURL is the value the proxy environment keys are set to (activation
// lives outside this package; this is the one place that spells the URL).
func (c Config) ProxyURL() string {
	u := url.URL{Scheme: "http", Host: c.Addr}
	return u.String()
}
