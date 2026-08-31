package transport

import "testing"

// TestAllowlistTreatsOneIPLiteralAsOneHost. An IP literal reaches the matcher
// in four shapes -- bracketed or bare, with a port or without -- and the
// bracket is only ever a delimiter for the port that is not there. Keying on
// the written form made `[::1]` and `[::1]:443` two different hosts, so a lane
// configured with one form blind-tunnelled the other: the model call succeeds
// and is never recorded, which is the same silent governance hole the DNS
// reductions exist to close.
func TestAllowlistTreatsOneIPLiteralAsOneHost(t *testing.T) {
	for _, class := range [][]string{
		{"[::1]", "[::1]:443", "::1", "[0:0:0:0:0:0:0:1]:8443", "0:0:0:0:0:0:0:1"},
		{"127.0.0.1", "127.0.0.1:443", "127.0.0.1:8443"},
		{"[2001:db8::1]", "[2001:DB8::1]:443", "2001:db8:0:0:0:0:0:1"},
	} {
		for _, configured := range class {
			a := NewAllowlist(configured)
			for _, wire := range class {
				if !a.Allows(wire) {
					t.Errorf("NewAllowlist(%q).Allows(%q) = false, want true: these spell one host, "+
						"and a miss blind-tunnels the call so it is never recorded", configured, wire)
				}
			}
		}
	}
}

// TestAllowlistKeepsIPLiteralsApart is the other half: collapsing the written
// forms must not collapse distinct addresses. ::1 is not 127.0.0.1, and a name
// that merely looks like one is not an address at all.
func TestAllowlistKeepsIPLiteralsApart(t *testing.T) {
	a := NewAllowlist("[::1]")
	for _, wire := range []string{"127.0.0.1:443", "::2", "[::2]:443", "localhost:443", "1:443"} {
		if a.Allows(wire) {
			t.Errorf("NewAllowlist(\"[::1]\").Allows(%q) = true, want false: normalizing the written "+
				"form must not widen what is intercepted", wire)
		}
	}
}

// TestAllowlistFoldsDNSNamesInASCIIOnly. U+212A KELVIN SIGN lowercases to 'k'
// under Unicode rules, so strings.ToLower would make `api.anthropiK.com` match
// `api.anthropick.com`. DNS folding is ASCII-only, and routing IP literals
// through netip must not have moved names onto a Unicode path.
func TestAllowlistFoldsDNSNamesInASCIIOnly(t *testing.T) {
	kelvin := "K"
	if NewAllowlist("api.anthropick.com").Allows("api.anthropi" + kelvin + ".com:443") {
		t.Error("U+212A folded to 'k': DNS folding must stay ASCII-only")
	}
}
