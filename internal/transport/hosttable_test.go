package transport

import (
	"net"
	"strings"
	"testing"
)

// TestAuthHostsAreExcludedByConstruction asserts the auth exclusion directly
// rather than merely claiming it: the table lists no bare "anthropic.com" or
// "openai.com", so none of these ever match either consumer.
func TestAuthHostsAreExcludedByConstruction(t *testing.T) {
	excluded := []string{
		"auth.anthropic.com:443",
		"auth.openai.com:443",
		"platform.openai.com:443",
		"anthropic.com:443",
		"openai.com:443",
	}
	all := AllowlistFor("claude-code", "codex")
	pac := PACBody(DefaultAddr, "claude-code", "codex")
	for _, host := range excluded {
		if all.Allows(host) {
			t.Errorf("the union allowlist matches %q; auth hosts must be excluded outright", host)
		}
		if pacRoutesThrough(t, pac, strings.TrimSuffix(host, ":443")) {
			t.Errorf("the PAC body routes %q; auth hosts must never match it", host)
		}
	}
}

// TestNoTableEntryIsLoopbackOrPrivate: a loopback or RFC1918 row in the table
// would let the PAC route the relay to itself (config.go's requireLoopback
// treats loopback as the bind side, never a proxied destination).
func TestNoTableEntryIsLoopbackOrPrivate(t *testing.T) {
	for _, r := range Union("claude-code", "codex") {
		ip := net.ParseIP(r.Host)
		if ip == nil {
			continue // a DNS name, not a literal; nothing to check here
		}
		if ip.IsLoopback() || ip.IsPrivate() {
			t.Errorf("host table entry %q parses as loopback/private; it must never be able to route "+
				"the relay to itself", r.Host)
		}
	}
}

// TestPACRoutesThroughTheRelaysOwnAddr uses a non-default addr, so the
// generator is proven to take it rather than hardcode the default port.
func TestPACRoutesThroughTheRelaysOwnAddr(t *testing.T) {
	const addr = "127.0.0.1:19999"
	body := PACBody(addr, "claude-code")
	if !strings.Contains(body, "PROXY "+addr+"; DIRECT") {
		t.Errorf("PAC body does not route through the configured addr %q:\n%s", addr, body)
	}
	if strings.Contains(body, DefaultAddr) {
		t.Errorf("PAC body names the default addr %q instead of the configured one %q:\n%s", DefaultAddr, addr, body)
	}
	if !strings.Contains(body, `return "DIRECT";`) {
		t.Errorf("PAC body has no DIRECT fallback:\n%s", body)
	}
}

// TestPACBodyLowercasesHostFirst: a PAC evaluator does not fold case, so the
// generated arms must lowercase the host before every comparison.
func TestPACBodyLowercasesHostFirst(t *testing.T) {
	body := PACBody(DefaultAddr, "claude-code")
	if !strings.Contains(body, "host = host.toLowerCase();") {
		t.Errorf("PAC body does not lowercase host first:\n%s", body)
	}
}

// TestPACBodyEmptyUnionIsAllDirect: an empty or unrecognized provider set
// must fail toward DIRECT, the same safe direction the allowlist's zero
// value takes, never toward proxying everything.
func TestPACBodyEmptyUnionIsAllDirect(t *testing.T) {
	body := PACBody(DefaultAddr)
	if strings.Contains(body, "PROXY") {
		t.Errorf("PAC body with no providers still names a PROXY arm:\n%s", body)
	}
	if strings.Count(body, "return") != 1 {
		t.Errorf("PAC body with no providers should have exactly the DIRECT fallback return:\n%s", body)
	}
}

// pacRoutesThrough is a tiny PAC interpreter for exactly the two shapes
// PACBody emits, so the pinning test below can assert the PAC's OWN decision
// rather than re-deriving it from the table it is supposed to be generated
// from.
func pacRoutesThrough(t *testing.T, body, host string) bool {
	t.Helper()
	host = asciiLower(host)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `if (host === "`) {
			continue
		}
		// Extract every quoted string on the line; the first is the exact host,
		// an optional second (from a dnsDomainIs arm) is ".host".
		var quoted []string
		rest := line
		for {
			i := strings.Index(rest, `"`)
			if i < 0 {
				break
			}
			rest = rest[i+1:]
			j := strings.Index(rest, `"`)
			if j < 0 {
				break
			}
			quoted = append(quoted, rest[:j])
			rest = rest[j+1:]
		}
		if len(quoted) < 1 {
			continue
		}
		exact := quoted[0]
		if host == exact {
			return true
		}
		if len(quoted) >= 2 && strings.HasSuffix(host, quoted[1]) {
			return true
		}
	}
	return false
}

// TestHostTablePinsPACAllowlistAndUnion pins that the PAC body and the
// allowlist always agree with the provider union, over three unions
// (claude-code only, codex only, both), including the both->claude-code-only
// shrink case and the no-loopback assertion. Two consumers, not three -- the
// CA carries no host bound of its own to keep in step.
func TestHostTablePinsPACAllowlistAndUnion(t *testing.T) {
	unions := map[string][]string{
		"claude-code only": {"claude-code"},
		"codex only":       {"codex"},
		"both":             {"claude-code", "codex"},
	}

	for name, providers := range unions {
		t.Run(name, func(t *testing.T) {
			rows := Union(providers...)
			if len(rows) == 0 {
				t.Fatalf("Union(%v) returned nothing", providers)
			}
			allow := AllowlistFor(providers...)
			pac := PACBody(DefaultAddr, providers...)

			for _, r := range rows {
				exact := r.Host + ":443"
				if !allow.Allows(exact) {
					t.Errorf("allowlist does not allow %q, which is in the union", exact)
				}
				if !pacRoutesThrough(t, pac, r.Host) {
					t.Errorf("PAC does not route %q, which is in the union:\n%s", r.Host, pac)
				}
				if r.IncludeSubdomains {
					sub := "sub." + r.Host + ":443"
					if !allow.Allows(sub) {
						t.Errorf("allowlist does not allow subdomain %q of suffix row %q", sub, r.Host)
					}
					if !pacRoutesThrough(t, pac, "sub."+r.Host) {
						t.Errorf("PAC does not route subdomain %q of suffix row %q:\n%s", sub, r.Host, pac)
					}
				}
			}
		})
	}

	// The shrink case: both -> claude-code only drops the OpenAI hosts from
	// BOTH consumers while keeping the Anthropic ones, rather than emptying
	// either (the uninstall-one-of-two case this table records and acts on).
	t.Run("both to claude-code-only shrinks rather than empties", func(t *testing.T) {
		bothAllow := AllowlistFor("claude-code", "codex")
		bothPAC := PACBody(DefaultAddr, "claude-code", "codex")
		ccAllow := AllowlistFor("claude-code")
		ccPAC := PACBody(DefaultAddr, "claude-code")

		if !bothAllow.Allows("api.openai.com:443") || !pacRoutesThrough(t, bothPAC, "api.openai.com") {
			t.Fatal("setup: the 'both' union does not cover OpenAI, so the shrink below proves nothing")
		}

		if ccAllow.Allows("api.openai.com:443") {
			t.Error("dropping codex still allows api.openai.com in the allowlist")
		}
		if pacRoutesThrough(t, ccPAC, "api.openai.com") {
			t.Errorf("dropping codex still routes api.openai.com in the PAC:\n%s", ccPAC)
		}
		if !ccAllow.Allows("api.anthropic.com:443") {
			t.Error("dropping codex also dropped api.anthropic.com from the allowlist; it must shrink, not empty")
		}
		if !pacRoutesThrough(t, ccPAC, "api.anthropic.com") {
			t.Errorf("dropping codex also dropped api.anthropic.com from the PAC; it must shrink, not empty:\n%s", ccPAC)
		}
	})

	// No entry parses as a loopback or private IP in either consumer.
	t.Run("no loopback or private literal in either consumer", func(t *testing.T) {
		for _, host := range []string{"127.0.0.1", "::1", "0.0.0.0", "192.168.1.1", "10.0.0.1"} {
			if AllowlistFor("claude-code", "codex").Allows(host + ":443") {
				t.Errorf("the union allowlist allows loopback/private literal %q", host)
			}
			if pacRoutesThrough(t, PACBody(DefaultAddr, "claude-code", "codex"), host) {
				t.Errorf("the union PAC routes loopback/private literal %q", host)
			}
		}
	})

	// Auth hosts never match in any of the three unions.
	for name, providers := range unions {
		allow := AllowlistFor(providers...)
		pac := PACBody(DefaultAddr, providers...)
		for _, auth := range []string{"auth.anthropic.com", "auth.openai.com", "platform.openai.com"} {
			if allow.Allows(auth + ":443") {
				t.Errorf("[%s] allowlist matches auth host %q", name, auth)
			}
			if pacRoutesThrough(t, pac, auth) {
				t.Errorf("[%s] PAC routes auth host %q", name, auth)
			}
		}
	}
}

// TestUnionIsStableAndDeduplicated: calling Union twice with the same
// providers must produce the same rows in the same order (PACBody's arms
// must not shuffle between calls), and a provider named twice must not
// duplicate its rows.
func TestUnionIsStableAndDeduplicated(t *testing.T) {
	a := Union("claude-code", "codex")
	b := Union("claude-code", "codex")
	if len(a) != len(b) {
		t.Fatalf("Union is not stable across calls: %v vs %v", a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("Union order changed between calls at index %d: %v vs %v", i, a[i], b[i])
		}
	}
	dup := Union("claude-code", "claude-code")
	if len(dup) != len(Union("claude-code")) {
		t.Errorf("naming a provider twice duplicated its rows: %v", dup)
	}
}

// TestRowsForUnrecognizedProviderIsEmpty: an unrecognized provider name must
// not panic and must contribute nothing, since Union treats it the same way.
func TestRowsForUnrecognizedProviderIsEmpty(t *testing.T) {
	if got := RowsFor("not-a-real-provider"); got != nil {
		t.Errorf("RowsFor(unknown) = %v, want nil", got)
	}
	if got := Union("not-a-real-provider"); got != nil {
		t.Errorf("Union(unknown) = %v, want nil", got)
	}
}

// TestRowsForReturnsACopy: a caller mutating what RowsFor returns must not be
// able to widen a later Union or AllowlistFor call.
func TestRowsForReturnsACopy(t *testing.T) {
	rows := RowsFor("claude-code")
	if len(rows) == 0 {
		t.Fatal("RowsFor(claude-code) returned nothing")
	}
	rows[0].Host = "evil.test"
	if AllowlistFor("claude-code").Allows("evil.test:443") {
		t.Error("mutating RowsFor's returned slice widened what AllowlistFor later builds")
	}
}
