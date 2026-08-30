package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/textproto"
	"strings"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/internal/decision"
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

// maxCaptureInputBytes bounds what is handed to the REDACTOR, which is a
// separate bound from the wire cap and exists for a different reason.
//
// The redactor runs eleven full regex passes plus an entropy walk over whatever
// it is given, and on the request path it is given the whole relayed body —
// bounded only by maxRequestBody (64 MiB). Measured on this machine, redacting a
// 64 MiB body takes ~11.4s of CPU, and 32 MiB takes ~5.7s, to produce a result
// that capRunes then truncates to 65,536 runes. That work sits SYNCHRONOUSLY in
// front of the forward and in front of the gate's verdict, on a listener that
// performs no caller authentication, so it is both a per-call latency cost and
// an amplification any local caller can aim.
//
// The response path was already bounded this way by captureSink; this is the
// same bound applied to the direction that lacked it.
//
// It must stay LARGER than captureBodyRunes or the wire cap becomes vacuous —
// the relationship maxThinkingBytes has to capBody in the client, and
// TestCaptureInputBoundExceedsTheWireCap is its control. 4x leaves room for the
// case where redaction GROWS a body: a placeholder is longer than the shortest
// value it replaces, so 65,536 runes of output can derive from fewer input
// bytes.
//
// The cost, stated rather than hidden: this is a truncation BEFORE redaction,
// and the ordering comment above is explicit that capping first can slice a
// secret in half. What that risks concretely is the one multiline pattern — a
// PEM block straddling this boundary loses its END anchor, so the retained head
// is base64 that no named pattern matches and that the entropy pass declines
// (it is not in a value position). For that head to reach the wire, the ~256 KiB
// in front of it would also have to redact down below the 65,536-rune cap. A PEM
// key is ~2-3 KiB, so the window is ~80x the object it could bisect.
const maxCaptureInputBytes = 4 * captureBodyRunes

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
// The input bound is applied HERE rather than at the two call sites, because
// CaptureRequest, Complete and Capture are all exported and each takes a caller's
// string. One funnel means a new caller cannot be the one that forgets it.
func captureBody(body string) string {
	if body == "" {
		return ""
	}
	if len(body) > maxCaptureInputBytes {
		body = body[:maxCaptureInputBytes]
	}
	// Trim an incomplete trailing rune. A mid-rune cut leaves an invalid UTF-8
	// tail that json.Marshal silently rewrites to U+FFFD, so the stored evidence
	// would end in a character the exchange never contained.
	//
	// Unconditional, NOT only when this function truncated. Callers pre-truncate
	// on a raw byte boundary before the body ever gets here — capturableBody cuts
	// at exactly maxCaptureInputBytes and captureSink fills to exactly its cap —
	// which lands the body AT the bound, not over it. A guard that only fired on
	// `len > max` therefore never ran for the one producer that actually cuts, and
	// the broken tail reached the wire. Doing it on every body also costs nothing:
	// the scan is bounded to the last utf8.UTFMax bytes.
	body = trimPartialRune(body)
	redacted, _, _ := bodyRedactor.RedactText(body)
	return capRunes(redacted)
}

// trimPartialRune drops a trailing byte sequence that is the start of a rune the
// string does not finish. A complete final rune, and an invalid byte that is not
// a truncation artefact, are both left alone: this repairs cuts, it does not
// sanitize.
func trimPartialRune(s string) string {
	for i := len(s) - 1; i >= 0 && i > len(s)-utf8.UTFMax; i-- {
		if !utf8.RuneStart(s[i]) {
			continue
		}
		if r, size := utf8.DecodeRuneInString(s[i:]); r == utf8.RuneError && size <= 1 {
			return s[:i]
		}
		return s
	}
	return s
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
// the client's stripContent, following the precedent that decision set for
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

// RequestCapture is the request half of the evidence, done before forwarding.
//
// It exists because the gate has to decide BEFORE a response exists, while the
// span needs both halves. Without the split, a caller would have to run Capture
// twice — once with zeroed response fields to feed the gate, once fully populated
// for the span — redoing the fingerprint and the redaction, and giving the
// ordering invariant two places to be got wrong instead of one.
type RequestCapture struct {
	// Fingerprint is taken from the LIVE headers, before Headers below was
	// redacted. That ordering is the whole reason this type holds both.
	Fingerprint string
	Headers     map[string]string
	Body        string
	Method      string
	URL         string
}

// CaptureRequest does the request half, in the one order that works.
//
// The sequence is not a style choice and the ordering test is its control: the
// fingerprint is taken from the LIVE request headers, before redactHeaders
// replaces the value it derives from. Reversing these two lines yields a
// fingerprint of the literal placeholder — the same value for every developer in
// every org, present on every span, and wrong in a way nothing else would show.
func CaptureRequest(method, url string, reqHeaders http.Header, reqBody string) RequestCapture {
	// 1. Fingerprint FIRST, from the untouched headers.
	fingerprint := credentialFingerprint(reqHeaders)

	// 2. Redact. Headers by key name; bodies through the shared detector.
	// 3. Cap — inside captureBody, after its redaction, never before.
	return RequestCapture{
		Fingerprint: fingerprint,
		Headers:     redactHeaders(reqHeaders),
		Body:        captureBody(reqBody),
		Method:      method,
		URL:         stripQuery(url),
	}
}

// Complete joins the response half onto an already-captured request.
//
// The request half is NOT recomputed: it was done once, before forwarding, and
// redoing it here would mean fingerprinting headers that have already been
// redacted — the exact inversion CaptureRequest exists to prevent.
func (r RequestCapture) Complete(status int, respHeaders http.Header, respBody string) Captured {
	return Captured{
		CredentialFingerprint: r.Fingerprint,
		RequestHeaders:        r.Headers,
		ResponseHeaders:       redactHeaders(respHeaders),
		RequestBody:           r.Body,
		ResponseBody:          captureBody(respBody),
		HTTPMethod:            r.Method,
		HTTPURL:               r.URL,
		HTTPStatus:            status,
	}
}

// ForGate renders the request half as a Captured for the gate's evaluation, whose
// verdict must be obtained before a response exists.
//
// The response fields are absent rather than zeroed-and-meaningful: a gate reading
// HTTPStatus 0 as "status zero" would be reading a value nothing measured.
func (r RequestCapture) ForGate() Captured {
	return Captured{
		CredentialFingerprint: r.Fingerprint,
		RequestHeaders:        r.Headers,
		RequestBody:           r.Body,
		HTTPMethod:            r.Method,
		HTTPURL:               r.URL,
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
