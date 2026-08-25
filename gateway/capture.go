package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/decision"
)

// capture.go turns the relay's tee into the evidence a governance event carries.
//
// The order is the control, and it is fixed: fingerprint -> redact -> cap.
//
//   - fingerprint FIRST, because it derives from the credential and the next step
//     removes it. Computed after redaction it would hash the placeholder —
//     identical for every developer, and useless for account binding while still
//     looking present.
//   - redact SECOND. Headers go by KEY NAME, not by inspecting the value: a
//     provider credential need not match anything the secret detector recognises,
//     and this repo has already measured that the detector's reach is decided by
//     the keyword beside a value, not by the value's shape. Bodies additionally
//     go through the shared detector, because a prompt can carry anything.
//   - cap LAST, because capping first could slice a secret in half and let the
//     halves through.
//
// None of this touches the forwarded bytes. Redaction applies to the captured
// copy only — that separation is the whole design.

// redactedHeaderValue replaces a credential header's value. The KEY is kept: that
// a request carried an Authorization header is governance-relevant, and dropping
// the key would make an unauthenticated call indistinguishable from an
// authenticated one.
const redactedHeaderValue = "[redacted]"

// credentialHeaders are redacted by name. From phase 05's requirement 1, and a
// deliberately closed list — a name not here is treated as ordinary metadata, so
// additions belong in this constant rather than in a caller.
var credentialHeaders = map[string]bool{
	"Authorization":        true,
	"Proxy-Authorization":  true,
	"Cookie":               true,
	"Set-Cookie":           true,
	"X-Api-Key":            true,
	"Api-Key":              true,
	"X-Auth-Token":         true,
	"X-Amz-Security-Token": true,
}

// fingerprintOrder is the credential header preference, in order. Fixed rather
// than "whichever is present", so the fingerprint for one credential cannot
// change because an unrelated header appeared alongside it.
var fingerprintOrder = []string{"Authorization", "X-Api-Key", "Api-Key"}

// captureBodyRunes bounds a captured body. Runes, not bytes, to match the wire
// cap this eventually passes through, so non-ASCII content is measured the same
// way at both ends.
const captureBodyRunes = 65536

// fingerprintHexLen is how much of the digest is kept. Enough to distinguish the
// credentials one org registers; short enough that the value is obviously an
// identifier and not a secret.
const fingerprintHexLen = 32

// credentialFingerprint answers "which registered credential made this call"
// without carrying the credential. One-way over the raw header value.
//
// It is NOT gated by content capture. It is derived evidence, and account binding
// is a governance control — letting a privacy setting remove it would let an org
// opt out of being identified. What IS asserted, on outbound bytes, is that the
// raw value is absent while this is present.
func credentialFingerprint(h http.Header) string {
	for _, name := range fingerprintOrder {
		value := strings.TrimSpace(h.Get(name))
		if value == "" || value == redactedHeaderValue {
			continue
		}
		sum := sha256.Sum256([]byte(value))
		return hex.EncodeToString(sum[:])[:fingerprintHexLen]
	}
	return ""
}

// redactHeaders flattens headers for capture, replacing credential values by key
// name. Multi-valued headers are joined; the wire shape is one string per key.
func redactHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for name, values := range h {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if credentialHeaders[canonical] {
			out[canonical] = redactedHeaderValue
			continue
		}
		out[canonical] = strings.Join(values, ", ")
	}
	return out
}

// bodyRedactor is the SAME secret detector the hook path uses. Shared
// deliberately: a second implementation would drift, and this one's reach is
// already measured rather than assumed.
var bodyRedactor = decision.NewRedactor()

// captureBody redacts then caps a captured body, in that order.
func captureBody(body string) string {
	if body == "" {
		return ""
	}
	redacted, _, _ := bodyRedactor.RedactText(body)
	return capRunes(redacted)
}

// capRunes truncates to captureBodyRunes, counted in characters.
func capRunes(s string) string {
	if len(s) <= captureBodyRunes { // byte length ≤ cap ⇒ rune count ≤ cap
		return s
	}
	r := []rune(s)
	if len(r) <= captureBodyRunes {
		return s
	}
	return string(r[:captureBodyRunes])
}

// Captured is the evidence one relayed model call produces. It is the gateway's
// whole output besides the relay itself.
//
// Content-bearing fields are populated unconditionally and gated downstream by
// the client's stripContent, following the precedent ADR-0019 P3 set for
// thinking: gating a pure transform buys nothing on the wire and duplicates the
// posture decision in a second place, where the two can disagree.
// CredentialFingerprint is the exception and stays present either way — it is
// derived evidence for a governance control, not content.
type Captured struct {
	RequestHeaders        map[string]string
	ResponseHeaders       map[string]string
	RequestBody           string
	ResponseBody          string
	CredentialFingerprint string
	HTTPMethod            string
	HTTPURL               string
	HTTPStatus            int
}

// Capture builds the evidence for one relayed call, in the one order that works.
//
// The sequence is not a style choice and the ordering test is its control:
// fingerprint is taken from the LIVE request headers, before redactHeaders
// replaces the value it derives from. Reversing these two lines yields a
// fingerprint of the literal placeholder — the same value for every developer in
// every org, present on every span, and wrong in a way nothing else would show.
func Capture(method, url string, reqHeaders http.Header, reqBody string,
	status int, respHeaders http.Header, respBody string) Captured {
	// 1. Fingerprint FIRST, from the untouched headers.
	fingerprint := credentialFingerprint(reqHeaders)

	// 2. Redact. Headers by key name; bodies through the shared detector.
	// 3. Cap — inside captureBody, after its redaction, never before.
	return Captured{
		CredentialFingerprint: fingerprint,
		RequestHeaders:        redactHeaders(reqHeaders),
		ResponseHeaders:       redactHeaders(respHeaders),
		RequestBody:           captureBody(reqBody),
		ResponseBody:          captureBody(respBody),
		HTTPMethod:            method,
		HTTPURL:               stripQuery(url),
		HTTPStatus:            status,
	}
}

// stripQuery drops the query string from a captured URL.
//
// Core classifies a span by looking for an LLM domain in http.url, so the host
// and path have to survive; the query does not, and dropping it keeps a future
// provider that accepts content or a token as a query parameter from turning this
// structural field into an ungated content leak.
func stripQuery(url string) string {
	if i := strings.IndexAny(url, "?#"); i >= 0 {
		return url[:i]
	}
	return url
}
