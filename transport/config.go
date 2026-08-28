package transport

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"time"
)

// DefaultInterceptHost is the one host this lane terminates TLS for.
//
// Single-host interception is the bound that makes ADR-0021 §5's reversal
// defensible (OD2): every other CONNECT is blind-tunnelled, never decrypted.
// Widening this is a decision, not a config tweak — and note that the CA's name
// constraint is derived from this constant at generation time, so widening the
// allowlist alone does not widen what the CA can sign.
const DefaultInterceptHost = "api.anthropic.com"

// DefaultAddr is this lane's deterministic loopback listen address.
//
// 8790, one past telemetry's 8789 and two past the gateway's 8788. Three
// loopback daemons can be installed on one machine, and two sharing a port means
// whichever starts second cannot bind — which under launchd's KeepAlive is a
// restart loop rather than an error anyone sees.
const DefaultAddr = "127.0.0.1:8790"

// resolveTimeout bounds the name lookup in requireLoopback. Unbounded, a slow or
// unreachable DNS path stalls startup with no output.
const resolveTimeout = 2 * time.Second

// Config is what the transport lane needs to run.
type Config struct {
	// Addr is the proxy's listen address. It must resolve to loopback: this proxy
	// performs no caller authentication AND terminates TLS for the provider's
	// hostname, so a non-loopback listener would let anything on the network route
	// its model calls through this machine's CA. Strictly a stronger case than the
	// gateway's, which only relays.
	Addr string

	// Allowlist names the hosts that are TLS-terminated. Everything else is
	// blind-tunnelled. Empty means the default single host; an explicitly
	// configured set is kept as-is and never widened by Validate.
	Allowlist Allowlist

	// Upstream overrides where an intercepted request is forwarded. EMPTY is the
	// production value and means "derive it from the CONNECT host" (UpstreamFor),
	// which is the only correct rule for a relay that must not retarget a call.
	//
	// It is here for the same reason gateway.Config.Upstream is — address
	// configurability without a code change — and its only caller today is the
	// control test, which needs a destination that fails deterministically rather
	// than one that depends on this machine reaching the real provider. Stated
	// plainly rather than dressed up: a field with one test caller is worth having
	// only because the production rule it bypasses cannot otherwise be exercised
	// without live provider traffic in a unit test.
	Upstream string
}

// Validate fills defaults and rejects a configuration that would break the two
// invariants this lane rests on: loopback-only binding, and interception bounded
// to an allowlist.
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

// UpstreamFor is the absolute base URL an intercepted CONNECT target forwards to.
//
// The intercepted request arrives in ORIGIN-FORM over the tunnel (a path, plus a
// Host header), so the relay's upstream has to be reconstructed from the CONNECT
// target rather than read off the request. Getting it wrong sends the developer's
// live credential somewhere else — the same failure gateway.ServeHTTP's
// origin-form guard exists to prevent, one layer up.
//
// The port is carried when it is not 443, because dropping it would silently
// retarget the call to the default port.
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

// requireLoopback rejects any bind host that is not loopback.
//
// THIRD COPY, and the trigger telemetry/config.go named ("if a third lane needs
// it, that is the point to extract it") has now fired. It was still declined,
// deliberately: there is no module all three may import. gateway holds the
// credential path and telemetry links the collector tree, and the whole point of
// the per-module dependency guards is that those graphs stay apart — so a shared
// home would join exactly what the guards exist to separate, to save thirty
// lines. What guards against drift instead is local and specific:
// TestValidateRefusesANonLoopbackListener asserts the behaviour here rather than
// trusting that three copies stayed equal.
//
// The empty host (":8790") and the wildcards bind every interface, so they are
// refused BY NAME rather than left to the IP check — naming them produces an
// error that says which mistake was made.
//
// A NAME is resolved rather than trusted. "localhost" is only loopback because
// the resolver says so, and hosts files, DNS, nsswitch and interception agents
// can all say otherwise. Every returned address has to be loopback, not just the
// first.
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

// ProxyURL is the value the proxy environment keys are set to (phase 12 owns the
// activation itself; this is the one place that spells the URL).
func (c Config) ProxyURL() string {
	u := url.URL{Scheme: "http", Host: c.Addr}
	return u.String()
}
