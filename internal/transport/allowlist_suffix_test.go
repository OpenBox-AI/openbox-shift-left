package transport

import "testing"

// TestNormalizeHostBeforeAfterTable pins normalizeHost's four reductions and
// the ASCII-only fold against the exact inputs the exact-match tests already
// exercise: adding suffix matching to Allows must not have moved anything
// inside normalizeHost itself. Each pair is the "before" (today's
// known-correct output) locked down as the "after" this change must still
// produce.
func TestNormalizeHostBeforeAfterTable(t *testing.T) {
	cases := map[string]string{
		"api.anthropic.com:443":  "api.anthropic.com",
		"api.anthropic.com:8443": "api.anthropic.com",
		"api.anthropic.com":      "api.anthropic.com",
		"API.ANTHROPIC.COM:443":  "api.anthropic.com",
		"Api.Anthropic.Com:443":  "api.anthropic.com",
		"api.anthropic.com.:443": "api.anthropic.com",
		"[::1]":                  "::1",
		"[::1]:443":              "::1",
		"127.0.0.1:443":          "127.0.0.1",
		"::ffff:127.0.0.1":       "127.0.0.1",
		"":                       "",
	}
	for in, want := range cases {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q (before/after regression: suffix matching must "+
				"not have changed normalizeHost's own output)", in, got, want)
		}
	}
}

// TestSuffixMatchingRespectsLabelBoundaries asserts the label-boundary
// safeguard directly: a suffix row for "claude.ai" must match only itself
// and a genuine subdomain, never a host that merely ends with the same
// characters. Any of these matching terminates TLS for a host the CA was
// never meant to cover.
func TestSuffixMatchingRespectsLabelBoundaries(t *testing.T) {
	a := NewAllowlistFromRules(HostRule{Host: "claude.ai", IncludeSubdomains: true})

	for _, host := range []string{
		"claude.ai:443",
		"claude.ai",
		"www.claude.ai:443",
		"a.b.claude.ai:443",
	} {
		if !a.Allows(host) {
			t.Errorf("Allows(%q) = false, want true: the suffix row covers itself and its subdomains", host)
		}
	}

	for _, host := range []string{
		"evilclaude.ai:443",
		"xclaude.ai:443",
		"claude.ai.evil.com:443",
		"notclaude.ai:443",
		"claude.aisomething.com:443",
	} {
		if a.Allows(host) {
			t.Errorf("Allows(%q) = true, want false: this over-matches the \"claude.ai\" suffix row on "+
				"raw string suffix rather than a label boundary, which would terminate TLS for a host "+
				"the CA was never meant to cover", host)
		}
	}
}

// TestExactRowDoesNotGainSubdomains: an exact row (IncludeSubdomains false)
// must not accidentally match a subdomain the way a suffix row does.
func TestExactRowDoesNotGainSubdomains(t *testing.T) {
	a := NewAllowlistFromRules(HostRule{Host: "api.anthropic.com"})
	if a.Allows("console.anthropic.com:443") {
		t.Error("an exact-match row matched a different host under the same parent domain")
	}
	if a.Allows("sub.api.anthropic.com:443") {
		t.Error("an exact-match row matched a subdomain of itself; only IncludeSubdomains rows should")
	}
	if !a.Allows("api.anthropic.com:443") {
		t.Error("an exact-match row did not match its own host")
	}
}

// TestAllowlistHostsReportsSuffixRowsDistinctly: doctor and the startup log
// line must be able to tell a suffix row from an exact one in the flat
// string list Hosts() returns.
func TestAllowlistHostsReportsSuffixRowsDistinctly(t *testing.T) {
	a := NewAllowlistFromRules(
		HostRule{Host: "api.anthropic.com"},
		HostRule{Host: "claude.ai", IncludeSubdomains: true},
	)
	got := a.Hosts()
	want := map[string]bool{"api.anthropic.com": true, "*.claude.ai": true}
	if len(got) != len(want) {
		t.Fatalf("Hosts() = %q, want exactly %v", got, want)
	}
	for _, h := range got {
		if !want[h] {
			t.Errorf("Hosts() contained unexpected entry %q", h)
		}
	}
}

// TestNewAllowlistFromRulesDropsAnEmptyHost mirrors NewAllowlist's own guard:
// a stray empty rule must not become a wildcard.
func TestNewAllowlistFromRulesDropsAnEmptyHost(t *testing.T) {
	a := NewAllowlistFromRules(HostRule{Host: "", IncludeSubdomains: true}, HostRule{Host: "api.anthropic.com"})
	if a.Allows("evil.test:443") {
		t.Error("an empty suffix rule acted as a wildcard")
	}
	if !a.Allows("api.anthropic.com:443") {
		t.Error("the valid rule alongside the empty one was dropped too")
	}
}
