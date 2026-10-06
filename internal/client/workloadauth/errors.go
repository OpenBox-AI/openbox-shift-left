// Package workloadauth is the RS256 client-assertion workload-identity
// client: private-key handling, the public JWK and its RFC 7638 thumbprint,
// the strict bootstrap parse, the client assertion, the token exchange, the
// disk/memory cache and single-flight Authenticator. Imports stdlib and
// golang-jwt/jwt/v5 only.
package workloadauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"
)

// Stage names which half of the workload-identity exchange an Error came
// from, since bootstrap and the token exchange fail in different ways and a
// caller (the client's cache-invalidate-on-401 path, for one) needs to tell
// them apart.
type Stage string

const (
	StageBootstrap Stage = "bootstrap"
	StageExchange  Stage = "exchange"
)

// Error is workloadauth's one error shape. Reason is the bootstrap
// reason_code or Keycloak's "error" field, bounded to maxReasonLen bytes.
// Detail is Keycloak's "error_description", bounded to maxDetailLen bytes and
// never carried verbatim beyond that, or on a network fault the fixed class
// netCause names. Hint is a fixed, package-authored
// string (the clock-skew guidance on an exchange rejection); it never echoes
// server text. None of Reason, Detail or Hint is ever populated from the
// token, the assertion or the private key, and Error() never accepts an
// argument that could be.
type Error struct {
	Stage     Stage
	Status    int
	Reason    string
	Detail    string
	Hint      string
	Transient bool
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("workloadauth: ")
	b.WriteString(string(e.Stage))
	if e.Status != 0 {
		fmt.Fprintf(&b, " failed (status %d)", e.Status)
	} else {
		b.WriteString(" failed")
	}
	if e.Reason != "" {
		fmt.Fprintf(&b, ": reason=%s", e.Reason)
	}
	if e.Detail != "" {
		fmt.Fprintf(&b, " (%s)", e.Detail)
	}
	if e.Hint != "" {
		fmt.Fprintf(&b, "; %s", e.Hint)
	}
	if e.Transient {
		b.WriteString(" [transient]")
	}
	return b.String()
}

// maxReasonLen bounds Error.Reason: a bootstrap reason_code or Keycloak's
// "error" field, both short machine identifiers.
const maxReasonLen = 64

// maxDetailLen bounds Error.Detail: Keycloak's free-text
// "error_description", which this package never forwards verbatim past this
// length.
const maxDetailLen = 200

// clockSkewHint is the fixed guidance an exchange 400/401/403 carries: the
// assertion's 60s lifetime is the most common reason a correctly-built
// assertion is rejected, and it is a host-clock problem, not a key or
// document problem.
const clockSkewHint = "an assertion lives 60s; check NTP"

// boundString truncates s to at most max bytes, backing up to a rune
// boundary so free text from Keycloak never ends in a split UTF-8 sequence.
func boundString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// isTransientStatus reports whether status is a fault the caller should
// retry rather than treat as a permanent rejection: any 5xx (503
// verifier_unavailable included) or 429.
func isTransientStatus(status int) bool {
	return status >= 500 || status == 429
}

// netCause names the class of a transport failure with a fixed,
// package-authored string -- timeout, DNS, refused, reset, TLS -- so a
// "network fault" says which failure it was without echoing the error text.
func netCause(err error) string {
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var recordErr tls.RecordHeaderError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &dnsErr):
		return "dns lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.As(err, &certErr), errors.As(err, &unknownAuth), errors.As(err, &recordErr):
		return "tls handshake failed"
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return "timeout"
	}
	return "network error"
}
