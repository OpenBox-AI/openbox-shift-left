package transport

import "testing"

// TestAllowlistMatchesTheExactHost is the affirmative half: the one host this
// lane is permitted to terminate TLS for.
func TestAllowlistMatchesTheExactHost(t *testing.T) {
	a := NewAllowlist("api.anthropic.com")

	for _, host := range []string{
		"api.anthropic.com:443",
		"api.anthropic.com:8443", // a non-443 port is still the same host
		"api.anthropic.com",      // goproxy hands host:port, but do not depend on it
		"API.ANTHROPIC.COM:443",  // DNS is case-insensitive; same host
		"Api.Anthropic.Com:443",
		"api.anthropic.com.:443", // the FQDN root dot is the same name
	} {
		if !a.Allows(host) {
			t.Errorf("Allows(%q) = false, want true: this is the host the lane exists to intercept, "+
				"and a miss here is a silent governance hole — the call is blind-tunnelled and never recorded", host)
		}
	}
}

// TestAllowlistRefusesEverythingElse is the half that makes that decision
// reversal defensible: every other host is blind-tunnelled, uninspected.
//
// The confusables are the point. A CA that can impersonate the provider to this
// machine is the highest-value secret this product holds after the signing key
// (phase 11 security notes); a matcher that accepted a lookalike would let it be
// pointed somewhere the developer never agreed to.
//
// The two non-ASCII cases are BUILT rather than written as literals. A Cyrillic
// homoglyph pasted into source is invisible to a reviewer — which is the whole
// attack — and this repo already has a rule about not writing fixture bytes as
// literals when they can be derived (CLAUDE.md, the base64 test-vector case).
func TestAllowlistRefusesEverythingElse(t *testing.T) {
	a := NewAllowlist("api.anthropic.com")

	cyrillicA := "а" // U+0430 CYRILLIC SMALL LETTER A, a homoglyph for ASCII 'a'
	hosts := []string{
		"", // no host at all is not a match
		"anthropic.com:443",
		"anthropic.com",
		"console.anthropic.com:443",
		"api.anthropic.com.evil.test:443", // suffix attack
		"evil.test:443",                   // unrelated
		"notapi.anthropic.com:443",        // prefix attack
		"api-anthropic.com:443",           // hyphen for dot
		"api.anthropic.co:443",            // truncated TLD
		"xn--pi-fmc.anthropic.com:443",    // punycode, as a real resolver would send a confusable
		cyrillicA + "pi.anthropic.com:443",
		"api.anthropic.com\x00.evil.test:443", // NUL splice
		"api.anthropic.com/../evil.test:443",
		" api.anthropic.com:443", // leading space
		"api.anthropic.com :443", // embedded space
		"127.0.0.1:443",
		"localhost:443",
	}
	for _, host := range hosts {
		if a.Allows(host) {
			t.Errorf("Allows(%q) = true, want false: only the exact configured host may be "+
				"TLS-terminated; everything else must be blind-tunnelled", host)
		}
	}
}

// TestEmptyAllowlistAllowsNothing pins the zero value's direction.
//
// Same shape as telemetry Policy's zero value SUPPRESSING (phase 10): a
// half-built or misconfigured lane must fail toward doing nothing, not toward
// intercepting everything.
func TestEmptyAllowlistAllowsNothing(t *testing.T) {
	var a Allowlist
	for _, host := range []string{"api.anthropic.com:443", "anything:443", ""} {
		if a.Allows(host) {
			t.Errorf("zero-value Allowlist allowed %q; an unconfigured allowlist must intercept nothing", host)
		}
	}
	if NewAllowlist().Allows("api.anthropic.com:443") {
		t.Error("NewAllowlist() with no hosts allowed a host; an empty allowlist must intercept nothing")
	}
}

// TestAllowlistNormalizesItsOwnHosts keeps configuration from being the hole the
// matcher is not: a host configured with a port, a trailing dot or uppercase
// must still match the wire form.
func TestAllowlistNormalizesItsOwnHosts(t *testing.T) {
	for _, configured := range []string{
		"API.Anthropic.com",
		"api.anthropic.com.",
		"api.anthropic.com:443",
	} {
		if !NewAllowlist(configured).Allows("api.anthropic.com:443") {
			t.Errorf("NewAllowlist(%q) did not match the wire host api.anthropic.com:443", configured)
		}
	}
	// An empty configured entry must not become a wildcard, or a stray comma in
	// config silently intercepts every host.
	if NewAllowlist("", "api.anthropic.com").Allows("evil.test:443") {
		t.Error("an empty configured host acted as a wildcard")
	}
}

// TestAllowlistHostsIsAStableCopy: the doctor block and the log line both report
// what is intercepted, and a caller must not be able to widen the live matcher
// by mutating what it was handed.
func TestAllowlistHostsIsAStableCopy(t *testing.T) {
	a := NewAllowlist("api.anthropic.com")
	got := a.Hosts()
	if len(got) != 1 || got[0] != "api.anthropic.com" {
		t.Fatalf("Hosts() = %q, want [api.anthropic.com]", got)
	}
	got[0] = "evil.test"
	if a.Allows("evil.test:443") {
		t.Error("mutating the slice returned by Hosts() widened the live allowlist")
	}
}
