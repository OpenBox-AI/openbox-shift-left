package telemetry

import (
	"strings"
	"testing"
)

// TestValidateRequiresLoopback loopback-only is the one invariant this
// receiver rests on, and it is a security control rather than a preference:
// prompts, tool inputs and outputs, and full model request bodies all arrive
// here, over an endpoint with no authentication.
func TestValidateRequiresLoopback(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr string
		want string // substring of the required error; "" = must be accepted
	}{
		{"default", "", ""},
		{"explicit loopback v4", "127.0.0.1:8789", ""},
		{"loopback v6", "[::1]:8789", ""},
		{"another v4 loopback", "127.0.0.2:8789", ""},

		{"every interface", "0.0.0.0:8789", "binds every interface"},
		{"every interface v6", "[::]:8789", "binds every interface"},
		{"bare port", ":8789", "binds every interface"},
		{"routable address", "192.168.1.10:8789", "not loopback"},
		{"public address", "8.8.8.8:8789", "not loopback"},

		{"not host:port", "127.0.0.1", "is not host:port"},
		{"no port", "127.0.0.1:", "names no port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Addr: tc.addr}
			err := c.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate(%q) = %v, want accepted", tc.addr, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate(%q) accepted an address that must be refused", tc.addr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate(%q) = %q, want it to mention %q", tc.addr, err, tc.want)
			}
		})
	}
}

// TestValidateFillsTheDefault an empty address takes the default rather than
// the OS's "any interface" meaning.
func TestValidateFillsTheDefault(t *testing.T) {
	c := Config{}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.Addr != DefaultAddr {
		t.Errorf("Addr = %q, want the default %q", c.Addr, DefaultAddr)
	}
}

// TestDefaultPortAvoidsTheObviousCollisions the default port must not be the
// OTLP standard, and must not be the gateway's. 4318 is what any other
// collector on the machine already has: binding it either fails, or succeeds
// after that collector dies and silently swallows exports meant for it.
func TestDefaultPortAvoidsTheObviousCollisions(t *testing.T) {
	for _, taken := range []string{":4317", ":4318", ":8788"} {
		if strings.HasSuffix(DefaultAddr, taken) {
			t.Errorf("DefaultAddr %q uses %s, which another local service is likely to hold", DefaultAddr, taken)
		}
	}
	if !strings.HasPrefix(DefaultAddr, "127.0.0.1:") {
		t.Errorf("DefaultAddr %q is not explicitly loopback", DefaultAddr)
	}
}

// TestRequestBoundIsExplicitAndSane the request bound must be set, and must be
// well under the library's 20MiB default. It must also stay comfortably above
// the largest record this lane expects; model request bodies peaked at 566KB
// in the evidence run.
func TestRequestBoundIsExplicitAndSane(t *testing.T) {
	const libraryDefault = 20 * 1024 * 1024
	const largestObservedBody = 566 * 1024

	if MaxRequestBodyBytes >= libraryDefault {
		t.Errorf("MaxRequestBodyBytes = %d, which does not tighten the library's %d default",
			MaxRequestBodyBytes, libraryDefault)
	}
	if MaxRequestBodyBytes <= largestObservedBody {
		t.Errorf("MaxRequestBodyBytes = %d, below the largest body this lane has actually seen (%d); "+
			"the bound would drop real records", MaxRequestBodyBytes, largestObservedBody)
	}
}
