package gateway

import (
	"net"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
)

// TestListenerMustBeLoopback holds the caller boundary.
func TestListenerMustBeLoopback(t *testing.T) {
	rejected := []string{
		"0.0.0.0:8788",
		":8788",
		"[::]:8788",
		"192.168.1.10:8788",
		"10.0.0.5:8788",
		"0.0.0.0:0",
	}
	for _, addr := range rejected {
		t.Run("reject/"+addr, func(t *testing.T) {
			cfg := Config{Addr: addr, Upstream: DefaultUpstream}
			if err := cfg.Validate(); err == nil {
				t.Errorf("Validate accepted non-loopback listener %q", addr)
			}
		})
	}

	accepted := []string{
		"127.0.0.1:8788",
		"127.0.0.2:8788",
		"[::1]:8788",
		"localhost:8788",
	}
	for _, addr := range accepted {
		t.Run("accept/"+addr, func(t *testing.T) {
			cfg := Config{Addr: addr, Upstream: DefaultUpstream}
			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate rejected loopback listener %q: %v", addr, err)
			}
		})
	}
}

// TestNewRejectsNonLoopback proves the check is wired into construction rather
// than merely available on the config type -- asserting the struct is not
// asserting the behaviour.
func TestNewRejectsNonLoopback(t *testing.T) {
	if _, err := New(Config{Addr: "0.0.0.0:8788", Upstream: DefaultUpstream}); err == nil {
		t.Fatal("New accepted a listener bound to every interface")
	}
}

// TestConfigDefaults keeps the port deterministic.
func TestConfigDefaults(t *testing.T) {
	var cfg Config
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate on a zero config: %v", err)
	}
	if cfg.Addr != DefaultAddr {
		t.Errorf("Addr: got %q want %q", cfg.Addr, DefaultAddr)
	}
	if cfg.Upstream != DefaultUpstream {
		t.Errorf("Upstream: got %q want %q", cfg.Upstream, DefaultUpstream)
	}
	if !strings.HasPrefix(DefaultAddr, "127.0.0.1:") {
		t.Errorf("DefaultAddr %q is not loopback", DefaultAddr)
	}
}

// TestUpstreamMustBeAbsolute catches a misconfiguration that would otherwise
// surface as every request 502ing.
func TestUpstreamMustBeAbsolute(t *testing.T) {
	for _, upstream := range []string{"api.anthropic.com", "/v1", "://bad"} {
		cfg := Config{Addr: DefaultAddr, Upstream: upstream}
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate accepted non-absolute upstream %q", upstream)
		}
	}
}

// TestUpstreamTrailingSlashNormalised keeps path joining from producing
// "//v1".
func TestUpstreamTrailingSlashNormalised(t *testing.T) {
	cfg := Config{Addr: DefaultAddr, Upstream: "https://api.anthropic.com/"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Upstream != "https://api.anthropic.com" {
		t.Errorf("Upstream: got %q want the trailing slash trimmed", cfg.Upstream)
	}
}

// TestNameHostsAreResolvedNotTrusted covers the gap a string check cannot see.
func TestNameHostsAreResolvedNotTrusted(t *testing.T) {
	cfg := Config{Addr: "localhost:8788", Upstream: DefaultUpstream}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate rejected localhost: %v", err)
	}

	cfg = Config{Addr: "no-such-host.invalid:8788", Upstream: DefaultUpstream}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate accepted a listen host that does not resolve")
	}
}

// TestListenVerifiesWhatTheKernelReturned is the post-bind control.
func TestListenVerifiesWhatTheKernelReturned(t *testing.T) {
	memhttptest.RequireBind(t)
	listener, resolved, err := Listen(Config{Addr: "127.0.0.1:0", Upstream: DefaultUpstream})
	if err != nil {
		t.Fatalf("Listen on loopback: %v", err)
	}
	defer listener.Close()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener bound a %T, not TCP", listener.Addr())
	}
	if !addr.IP.IsLoopback() {
		t.Errorf("bound %s, which is not loopback", addr.IP)
	}

	if resolved.Upstream != DefaultUpstream {
		t.Errorf("Listen returned Upstream %q, want the validated default", resolved.Upstream)
	}

	if _, _, err := Listen(Config{Addr: "0.0.0.0:0", Upstream: DefaultUpstream}); err == nil {
		t.Error("Listen bound every interface")
	}
}

// TestSelfReferentialUpstreamRejected catches a configuration that would relay
// into itself: each forwarded request re-enters the gateway and spawns
// another, until goroutines or sockets run out.
func TestSelfReferentialUpstreamRejected(t *testing.T) {
	cfg := Config{Addr: "127.0.0.1:8788", Upstream: "http://127.0.0.1:8788"}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate accepted an upstream pointing at the gateway's own listener")
	}
}

// TestUpstreamTrailingSlashesAllTrimmed is the case one TrimSuffix missed: it
// strips a single slash, so "…com//" kept one and every request URL grew a
// doubled separator.
func TestUpstreamTrailingSlashesAllTrimmed(t *testing.T) {
	for _, in := range []string{
		"https://api.anthropic.com/",
		"https://api.anthropic.com//",
		"https://api.anthropic.com///",
	} {
		cfg := Config{Addr: DefaultAddr, Upstream: in}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate(%q): %v", in, err)
		}
		if cfg.Upstream != "https://api.anthropic.com" {
			t.Errorf("Upstream %q normalised to %q, want no trailing slash", in, cfg.Upstream)
		}
	}
}

// TestSelfLoopDetectedAcrossSpellings is the case a raw string comparison
// missed: the same loopback socket named two ways.
func TestSelfLoopDetectedAcrossSpellings(t *testing.T) {
	looping := []struct{ addr, upstream string }{
		{"127.0.0.1:8788", "http://127.0.0.1:8788"},
		{"localhost:8788", "http://127.0.0.1:8788"},
		{"127.0.0.1:8788", "http://localhost:8788"},
		{"localhost:8788", "http://localhost:8788"},
		{"[::1]:8788", "http://127.0.0.1:8788"},
		{"127.0.0.1:80", "http://localhost"},
	}
	for _, tc := range looping {
		cfg := Config{Addr: tc.addr, Upstream: tc.upstream}
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate accepted a self-loop: addr=%s upstream=%s", tc.addr, tc.upstream)
		}
	}

	fine := []struct{ addr, upstream string }{
		{"127.0.0.1:8788", DefaultUpstream},
		{"127.0.0.1:8788", "http://127.0.0.1:8899"},
		{"127.0.0.1:8788", "http://localhost:8899"},
	}
	for _, tc := range fine {
		cfg := Config{Addr: tc.addr, Upstream: tc.upstream}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate rejected a valid pair addr=%s upstream=%s: %v", tc.addr, tc.upstream, err)
		}
	}
}
