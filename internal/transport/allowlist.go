package transport

import (
	"net"
	"net/netip"
	"strings"
)

// Allowlist decides which CONNECT targets this lane may terminate TLS for.
// Interception is allowlisted to a single host, and that bound is what makes
// that decision reversal defensible (OD2, phase 11): every other CONNECT is
// blind-tunnelled; forwarded byte-for-byte, never decrypted, never captured.
type Allowlist struct {
	hosts map[string]struct{}
}

// NewAllowlist builds an allowlist over the given hosts. An empty entry is
// dropped rather than stored: a stray comma in configuration must not become a
// wildcard that intercepts every host on the machine.
func NewAllowlist(hosts ...string) Allowlist {
	a := Allowlist{hosts: make(map[string]struct{}, len(hosts))}
	for _, h := range hosts {
		if n := normalizeHost(h); n != "" {
			a.hosts[n] = struct{}{}
		}
	}
	return a
}

// Allows reports whether the CONNECT target may be TLS-terminated.
func (a Allowlist) Allows(target string) bool {
	if len(a.hosts) == 0 {
		return false
	}
	n := normalizeHost(target)
	if n == "" {
		return false
	}
	_, ok := a.hosts[n]
	return ok
}

// Hosts returns what this allowlist intercepts, for the doctor block and the
// startup log line.
func (a Allowlist) Hosts() []string {
	out := make([]string, 0, len(a.hosts))
	for h := range a.hosts {
		out = append(out, h)
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
