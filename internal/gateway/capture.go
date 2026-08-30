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

const redactedHeaderValue = "[redacted]"

// credentialHeaders from phase 05's requirement 1, and a deliberately closed
// list; a name not here is treated as ordinary metadata, so additions belong
// in this constant rather than in a caller.
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

// fingerprintOrder fixed rather than "whichever is present", so the
// fingerprint for one credential cannot change because an unrelated header
// appeared alongside it.
var fingerprintOrder = []string{"Authorization", "X-Api-Key", "Api-Key"}

const captureBodyRunes = 65536

// maxCaptureInputBytes 4x leaves room for the case where redaction grows a
// body: a placeholder is longer than the shortest value it replaces, so 65,536
// runes of output can derive from fewer input bytes.
const maxCaptureInputBytes = 4 * captureBodyRunes

const fingerprintHexLen = 32

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

// bodyRedactor shared deliberately: a second implementation would drift, and
// this one's reach is already measured rather than assumed.
var bodyRedactor = decision.NewRedactor()

// captureBody one funnel means a new caller cannot be the one that forgets it.
func captureBody(body string) string {
	if body == "" {
		return ""
	}
	if len(body) > maxCaptureInputBytes {
		body = body[:maxCaptureInputBytes]
	}
	body = trimPartialRune(body)
	redacted, _, _ := bodyRedactor.RedactText(body)
	return capRunes(redacted)
}

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

// Captured is the evidence one relayed model call produces.
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
type RequestCapture struct {
	// Fingerprint is taken from the live headers, before Headers below was
	// redacted.
	Fingerprint string
	Headers     map[string]string
	Body        string
	Method      string
	URL         string
}

// CaptureRequest does the request half, in the one order that works.
func CaptureRequest(method, url string, reqHeaders http.Header, reqBody string) RequestCapture {
	fingerprint := credentialFingerprint(reqHeaders)

	// Cap; inside captureBody, after its redaction, never before.
	return RequestCapture{
		Fingerprint: fingerprint,
		Headers:     redactHeaders(reqHeaders),
		Body:        captureBody(reqBody),
		Method:      method,
		URL:         stripQuery(url),
	}
}

// Complete joins the response half onto an already-captured request.
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

// ForGate renders the request half as a Captured for the gate's evaluation,
// whose verdict must be obtained before a response exists.
func (r RequestCapture) ForGate() Captured {
	return Captured{
		CredentialFingerprint: r.Fingerprint,
		RequestHeaders:        r.Headers,
		RequestBody:           r.Body,
		HTTPMethod:            r.Method,
		HTTPURL:               r.URL,
	}
}

func stripQuery(url string) string {
	if i := strings.IndexAny(url, "?#"); i >= 0 {
		return url[:i]
	}
	return url
}
