package transport

import (
	"strings"
	"testing"
)

// TestDefaultPortIsDistinctFromTheOtherLanes.
//
// Three loopback daemons can be installed on one machine. Two sharing a port
// means whichever starts second fails to bind — and under launchd's KeepAlive
// that is a restart loop, not a clean error the developer sees.
func TestDefaultPortIsDistinctFromTheOtherLanes(t *testing.T) {
	// Written as literals rather than imported: gateway and telemetry are not
	// direct dependencies of this module (its guard allows one), and hard-coding
	// them here is what makes a future collision show up as a failing test in the
	// module that moved rather than as a silent restart loop in the field.
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
//
// This proxy performs no caller authentication and terminates TLS for the
// provider's hostname. A non-loopback listener would let anything on the network
// route its model calls through this machine's CA — the same reasoning as
// gateway.Config.Validate, and a stronger case: the gateway only relays, this
// also decrypts.
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

// TestUpstreamForIsFixedPerHost.
//
// The intercepted request arrives in origin-form over the tunnel, so the upstream
// base URL has to be reconstructed from the CONNECT host. Getting this wrong
// would send the developer's credential somewhere else — the same failure
// gateway.ServeHTTP's origin-form guard exists to prevent.
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
	// A non-443 port must be carried, or the relay would silently retarget the
	// call to the default port.
	if got := UpstreamFor("api.anthropic.com:8443"); got != "https://api.anthropic.com:8443" {
		t.Errorf("UpstreamFor with a non-default port = %q, want the port preserved", got)
	}
}
