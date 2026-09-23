package transport

import (
	"net"
	"net/netip"
	"strings"
)

// Allowlist decides which CONNECT targets this lane may terminate TLS for.
// Interception is allowlisted to the installed providers' host table union,
// covering every row a provider names, not a single host. A freshly
// minted CA carries no constraint of its own (owner ruling 2026-09-22), so
// this allowlist -- combined with CA.CanIssueFor for a machine still holding
// a legacy constrained CA -- is what makes that decision reversal defensible:
// every CONNECT this allowlist does not name is blind-tunnelled; forwarded
// byte-for-byte, never decrypted, never captured.
type Allowlist struct {
	hosts    map[string]struct{}
	suffixes map[string]struct{}
}

// NewAllowlist builds an exact-match allowlist over the given hosts. An empty
// entry is dropped rather than stored: a stray comma in configuration must
// not become a wildcard that intercepts every host on the machine.
func NewAllowlist(hosts ...string) Allowlist {
	a := Allowlist{hosts: make(map[string]struct{}, len(hosts))}
	for _, h := range hosts {
		if n := normalizeHost(h); n != "" {
			a.hosts[n] = struct{}{}
		}
	}
	return a
}

// NewAllowlistFromRules builds an allowlist over host table rows: an exact
// row matches only itself; a suffix row (IncludeSubdomains) also matches
// every subdomain under it. An empty host is dropped, same as NewAllowlist.
func NewAllowlistFromRules(rules ...HostRule) Allowlist {
	a := Allowlist{hosts: make(map[string]struct{}), suffixes: make(map[string]struct{})}
	for _, r := range rules {
		n := normalizeHost(r.Host)
		if n == "" {
			continue
		}
		if r.IncludeSubdomains {
			a.suffixes[n] = struct{}{}
		} else {
			a.hosts[n] = struct{}{}
		}
	}
	return a
}

// Allows reports whether the CONNECT target may be TLS-terminated: an exact
// hit, or -- for a suffix row -- the target itself or a subdomain of it. The
// suffix walk requires the preceding byte to be a literal '.', so
// "evilclaude.ai" and "claude.ai.evil.com" cannot match a "claude.ai" suffix
// row; a label-boundary miss here would terminate TLS for a host the CA was
// never meant to cover.
func (a Allowlist) Allows(target string) bool {
	n := normalizeHost(target)
	if n == "" {
		return false
	}
	if _, ok := a.hosts[n]; ok {
		return true
	}
	for suffix := range a.suffixes {
		if onLabelBoundary(n, suffix) {
			return true
		}
	}
	return false
}

// onLabelBoundary reports whether normalized host n is suffix itself or a
// subdomain of it. The byte before the match must be a literal '.', so
// "evilclaude.ai" never matches "claude.ai". Allowlist.Allows and
// CA.CanIssueFor share it: a fix to one boundary rule that missed the other
// would let the relay intercept a host the CA cannot mint for, or the reverse.
func onLabelBoundary(n, suffix string) bool {
	return n == suffix || strings.HasSuffix(n, "."+suffix)
}

// Hosts returns what this allowlist intercepts, for the doctor block and the
// startup log line: exact hosts as themselves, suffix rows prefixed "*." so
// the two are told apart in a flat string list.
func (a Allowlist) Hosts() []string {
	out := make([]string, 0, len(a.hosts)+len(a.suffixes))
	for h := range a.hosts {
		out = append(out, h)
	}
	for s := range a.suffixes {
		out = append(out, "*."+s)
	}
	return out
}

// normalizeHost four reductions, each because one host arrives in more than one
// shape and a miss is a silent governance hole: an unmatched host is
// blind-tunnelled, so the call succeeds and is never recorded. The port goes,
// one trailing root dot goes, ASCII letters lowercase, and an IP literal
// reduces to netip's canonical text -- which is what makes "[::1]", "[::1]:443"
// and "0:0:0:0:0:0:0:1" one key rather than three. Unmap folds
// "::ffff:127.0.0.1" in with "127.0.0.1", which reaches the same endpoint.
//
// Names keep the ASCII fold: U+212A KELVIN SIGN lowercases to 'k' under Unicode
// rules, so strings.ToLower would let "anthropiK" match "anthropick". A zone
// survives on purpose, because fe80::1%eth0 and %eth1 are two interfaces.
func normalizeHost(target string) string {
	if ap, err := netip.ParseAddrPort(target); err == nil {
		return ap.Addr().Unmap().String()
	}
	h := target
	if host, _, err := net.SplitHostPort(target); err == nil {
		h = host
	}
	h = strings.TrimSuffix(h, ".")
	if len(h) > 1 && h[0] == '[' && h[len(h)-1] == ']' {
		h = h[1 : len(h)-1] // bare literal written with the port's brackets
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		return addr.Unmap().String()
	}
	return asciiLower(h)
}

func asciiLower(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if b == nil {
				b = []byte(s)
			}
			b[i] = c + ('a' - 'A')
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}
