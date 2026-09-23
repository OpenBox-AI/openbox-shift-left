package transport

import (
	"strings"
	"testing"
)

// TestDefaultPortIsDistinctFromTheOtherLanes.
func TestDefaultPortIsDistinctFromTheOtherLanes(t *testing.T) {
	const gatewayAddr = "127.0.0.1:8788"
	const telemetryAddr = "127.0.0.1:8789"

	if DefaultAddr == gatewayAddr || DefaultAddr == telemetryAddr {
		t.Fatalf("DefaultAddr %q collides with another lane (gateway %q, telemetry %q)",
			DefaultAddr, gatewayAddr, telemetryAddr)
	}
}

// TestValidateFillsDefaults.
func TestValidateFillsDefaults(t *testing.T) {
	var c Config
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate on a zero Config: %v", err)
	}
	if c.Addr != DefaultAddr {
		t.Errorf("Addr = %q, want the default %q", c.Addr, DefaultAddr)
	}
	if !c.Allowlist.Allows(DefaultInterceptHost + ":443") {
		t.Errorf("the default configuration does not intercept %q, so the lane would capture nothing",
			DefaultInterceptHost)
	}
}

// TestValidateRefusesANonLoopbackListener.
func TestValidateRefusesANonLoopbackListener(t *testing.T) {
	for _, addr := range []string{
		"0.0.0.0:8790",
		"192.168.1.10:8790",
		"[::]:8790",
		"example.test:8790",
	} {
		c := Config{Addr: addr}
		err := c.Validate()
		if err == nil {
			t.Errorf("Validate accepted the non-loopback listen address %q", addr)
			continue
		}
		if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("Validate(%q) error %q does not say why; the developer has to be told it is the "+
				"listen address, not the proxy settings", addr, err)
		}
	}
}

// TestValidateAcceptsLoopbackForms.
func TestValidateAcceptsLoopbackForms(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:8790",
		"localhost:8790",
		"[::1]:8790",
	} {
		c := Config{Addr: addr}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate rejected the loopback address %q: %v", addr, err)
		}
	}
}

// TestValidateRefusesAMalformedListenAddress.
func TestValidateRefusesAMalformedListenAddress(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "not a host:port:8790", ":::8790"} {
		c := Config{Addr: addr}
		if err := c.Validate(); err == nil {
			t.Errorf("Validate accepted the malformed listen address %q", addr)
		}
	}
}

// TestValidateKeepsAnExplicitAllowlist: a caller that configured hosts keeps
// them; Validate must not silently widen or replace the set.
func TestValidateKeepsAnExplicitAllowlist(t *testing.T) {
	c := Config{Allowlist: NewAllowlist("example.test")}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !c.Allowlist.Allows("example.test:443") {
		t.Error("Validate dropped the configured allowlist")
	}
	if c.Allowlist.Allows(DefaultInterceptHost + ":443") {
		t.Errorf("Validate ADDED %q to an explicitly configured allowlist; widening what is "+
			"TLS-intercepted must never be a side effect of validation", DefaultInterceptHost)
	}
}

// TestValidateDefaultsNilProvidersToClaudeCode: a Config whose Providers was
// never set (nil, the zero value) gets today's behaviour.
func TestValidateDefaultsNilProvidersToClaudeCode(t *testing.T) {
	var c Config
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(c.Providers) != 1 || c.Providers[0] != "claude-code" {
		t.Errorf("Providers = %v, want [claude-code] for a Config that never set it", c.Providers)
	}
	if !c.Allowlist.Allows(DefaultInterceptHost + ":443") {
		t.Errorf("the defaulted providers do not intercept %q", DefaultInterceptHost)
	}
}

// TestValidateKeepsAnExplicitlyEmptyProviderSetEmpty: an explicitly empty,
// non-nil Providers slice means every provider was uninstalled. Validate
// must not fall back to claude-code -- the union, the allowlist and (through
// the same providers value) the PAC must all end up empty, never the
// default host.
func TestValidateKeepsAnExplicitlyEmptyProviderSetEmpty(t *testing.T) {
	c := Config{Providers: []string{}}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(c.Providers) != 0 {
		t.Errorf("Providers = %v, want it to stay empty", c.Providers)
	}
	if c.Allowlist.Allows(DefaultInterceptHost + ":443") {
		t.Errorf("an explicitly empty Providers still intercepts %q; it must intercept nothing", DefaultInterceptHost)
	}
	if pac := PACBody(c.Addr, c.Providers...); strings.Contains(pac, "PROXY") {
		t.Errorf("PAC body for an explicitly empty provider set still names a PROXY arm:\n%s", pac)
	}
}

// TestValidateAnExplicitAllowlistDoesNotDefaultThePACProviders: a caller that
// supplies its own allowlist but no provider set must not get a PAC that
// routes the default provider's hosts its allowlist never named.
func TestValidateAnExplicitAllowlistDoesNotDefaultThePACProviders(t *testing.T) {
	c := Config{Allowlist: NewAllowlist("example.test")}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !c.Allowlist.Allows("example.test:443") {
		t.Errorf("Validate replaced the caller's allowlist")
	}
	if pac := PACBody(c.Addr, c.Providers...); strings.Contains(pac, "PROXY") {
		t.Errorf("PAC routes hosts the explicit allowlist never named:\n%s", pac)
	}
}

// TestUpstreamForIsFixedPerHost.
func TestUpstreamForIsFixedPerHost(t *testing.T) {
	cases := map[string]string{
		"api.anthropic.com:443":  "https://api.anthropic.com",
		"api.anthropic.com":      "https://api.anthropic.com",
		"API.ANTHROPIC.COM:443":  "https://api.anthropic.com",
		"api.anthropic.com.:443": "https://api.anthropic.com",
	}
	for in, want := range cases {
		if got := UpstreamFor(in); got != want {
			t.Errorf("UpstreamFor(%q) = %q, want %q", in, got, want)
		}
	}
	if got := UpstreamFor("api.anthropic.com:8443"); got != "https://api.anthropic.com:8443" {
		t.Errorf("UpstreamFor with a non-default port = %q, want the port preserved", got)
	}
}
