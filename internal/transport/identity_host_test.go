package transport

import "testing"

// TestKeycloakIdentityHostIsNeverIntercepted pins that the relay never
// intercepts or reads the Keycloak identity host: the workload client's
// bootstrap and token exchange must reach the identity host (whichever
// Keycloak realm is configured) over a connection the transport lane does not
// see, which is what lets that client carry the API key and the signed
// assertion in the clear over their own TLS session rather than the
// relay's.
func TestKeycloakIdentityHostIsNeverIntercepted(t *testing.T) {
	const identityHost = "identity.example.com"

	if provider, ok := ProviderForHost(identityHost); ok {
		t.Fatalf("ProviderForHost(%q) = %q, true; want no provider to claim the identity host", identityHost, provider)
	}

	// hostTableProviders, not provider.Supported(): this package's depguard
	// allowlist admits no repo-local import beyond internal/gateway, so the
	// provider set is this package's own unexported list, the same way
	// ProviderForHost derives it (hosttable.go).
	for _, host := range []string{identityHost, "keycloak.example.com"} {
		allow := AllowlistFor(hostTableProviders...)
		if allow.Allows(host) {
			t.Errorf("AllowlistFor(hostTableProviders...) allows %q; the identity host must never be interceptable", host)
		}

		body := PACBody("127.0.0.1:0", hostTableProviders...)
		if containsHost(body, host) {
			t.Errorf("PACBody names %q; the identity host must never be routed through the relay", host)
		}
	}
}

// containsHost is a small, dependency-free substring check: the PAC body is
// generated JS, and a host name that appears there at all is enough to prove
// the union covers it.
func containsHost(body, host string) bool {
	for i := 0; i+len(host) <= len(body); i++ {
		if body[i:i+len(host)] == host {
			return true
		}
	}
	return false
}
