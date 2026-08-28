package transport

import (
	"net"
	"strings"
)

// Allowlist decides which CONNECT targets this lane may terminate TLS for.
//
// Interception is allowlisted to a single host, and that bound is what makes the
// ADR-0021 §5 reversal defensible (OD2, phase 11): every other CONNECT is
// blind-tunnelled — forwarded byte-for-byte, never decrypted, never captured.
// Widening it is a decision, not a config tweak.
//
// The zero value allows NOTHING. That direction is deliberate and matches
// telemetry's Policy zero value suppressing (phase 10): a half-built or
// misconfigured lane must fail toward doing nothing, never toward intercepting
// everything.
type Allowlist struct {
	// hosts is keyed by the normalized host, so lookup is a map hit with no
	// pattern language anywhere in the path. No wildcards, no regex, no suffix
	// rule — phase 11 requirement 5, and the reason is that every one of those
	// has a documented history of matching a lookalike.
	hosts map[string]struct{}
}

// NewAllowlist builds an allowlist over the given hosts.
//
// Configured hosts are normalized the same way wire hosts are, so an entry
// written with a port, a trailing dot or uppercase still matches. An EMPTY entry
// is dropped rather than stored: a stray comma in configuration must not become
// a wildcard that intercepts every host on the machine.
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
//
// The argument is goproxy's `host` — normally "host:port", but the port is not
// depended on.
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
//
// A COPY, because a caller that mutated the returned slice would otherwise be
// able to widen what the live matcher terminates TLS for.
func (a Allowlist) Hosts() []string {
	out := make([]string, 0, len(a.hosts))
	for h := range a.hosts {
		out = append(out, h)
	}
	return out
}

// normalizeHost reduces a host to the one form both sides compare in.
//
// Three reductions, each because the same DNS name legitimately arrives in more
// than one shape, and a miss is a SILENT governance hole — an unmatched host is
// blind-tunnelled, so the model call succeeds and is simply never recorded:
//
//   - the port is dropped ("api.anthropic.com:443" and ":8443" are one host);
//   - one trailing root dot is dropped (the FQDN form of the same name);
//   - ASCII letters are lowercased (DNS is case-insensitive).
//
// The lowercasing is ASCII-ONLY, and that is a security property rather than a
// shortcut. strings.ToLower folds Unicode, and some non-ASCII runes fold INTO
// ASCII letters — U+212A KELVIN SIGN lowercases to 'k' — so a Unicode-aware fold
// can turn a host that is not the provider's into one that compares equal to it.
// Byte-exact comparison after an ASCII-only fold cannot: any non-ASCII byte in
// the input survives the fold and fails the match. The confusable cases in
// allowlist_test.go are the control.
func normalizeHost(target string) string {
	h := target
	if host, _, err := net.SplitHostPort(target); err == nil {
		h = host
	}
	// One dot, not TrimRight: "api.anthropic.com.." is not the FQDN form of
	// anything, and collapsing it would accept a name no resolver would.
	h = strings.TrimSuffix(h, ".")
	return asciiLower(h)
}

// asciiLower lowercases A-Z and leaves every other byte exactly as it arrived.
// See normalizeHost for why this is not strings.ToLower.
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
