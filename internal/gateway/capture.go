package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/textproto"
	"strings"
	"time"
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

// captureRequestBody is the request path's funnel. It differs from captureBody by
// one step, and that step is a repair rather than a refinement.
//
// Redaction runs after selection and can GROW a body, because the
// ${OPENBOX_REDACTED_*} placeholder is a fixed ~37 bytes whatever it replaces. So
// growth comes from SHORT secrets, not long ones -- measured, `pwd:` plus eight
// characters goes 13 bytes to 42, x3.23 -- and a conversation dense in short
// keyword-adjacent values (a pasted .env, a secrets manifest) can outrun any fixed
// headroom. It did: a document selected to 49,115 bytes redacted to 83,770, and
// capRunes then head-kept 65,536 of them with no marker, leaving invalid JSON cut
// mid-token. That is the unmarked-head-window mode selection exists to remove,
// reappearing one layer downstream of it.
//
// A bigger budget cannot fix that -- it is an arms race against a multiplier --
// and giving this path a tail-preserving cap instead would cut a JSON document at
// a byte boundary and move the client's net. So the recovery is to SELECT AGAIN,
// once, on the redacted text: placeholders are plain ASCII inside string values,
// so the redacted document still binds, and re-selecting drops the messages that
// no longer fit and says so in the note. If redaction ever did break the JSON, the
// second pass degrades to its marked tail window, which is still honest.
//
// One pass suffices: the second selection measures already-redacted bytes, so
// nothing can grow after it. That makes both downstream caps unreachable rather
// than merely unlikely.
func captureRequestBody(body string) string {
	if body == "" {
		return ""
	}
	if len(body) > maxCaptureInputBytes {
		body = body[:maxCaptureInputBytes]
	}
	body = trimPartialRune(body)
	redacted, _, _ := bodyRedactor.RedactText(body)
	if len(redacted) > selectionBudget {
		redacted = selectModelCallRequest(redacted)
	}
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

	// StartedAt and EndedAt bound the RELAYED call, request capture to end-of-stream.
	StartedAt time.Time
	EndedAt   time.Time
}

func (c Captured) Elapsed() time.Duration {
	if c.StartedAt.IsZero() || !c.EndedAt.After(c.StartedAt) {
		return 0
	}
	return c.EndedAt.Sub(c.StartedAt)
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
	// At is when the request half was taken; a side channel would be a second place
	// for the two ends to disagree.
	At time.Time
}

// CaptureRequest does the request half, in the one order that works.
func CaptureRequest(method, url string, reqHeaders http.Header, reqBody string, at time.Time) RequestCapture {
	fingerprint := credentialFingerprint(reqHeaders)

	// Cap; inside captureRequestBody, after its redaction, never before.
	return RequestCapture{
		Fingerprint: fingerprint,
		Headers:     redactHeaders(reqHeaders),
		Body:        captureRequestBody(reqBody),
		Method:      method,
		URL:         stripQuery(url),
		At:          at,
	}
}

// Complete joins the response half on. `at` must be end-of-stream, not response
// headers: a streamed completion runs for seconds afterwards.
func (r RequestCapture) Complete(status int, respHeaders http.Header, respBody string, at time.Time) Captured {
	return Captured{
		CredentialFingerprint: r.Fingerprint,
		RequestHeaders:        r.Headers,
		ResponseHeaders:       redactHeaders(respHeaders),
		RequestBody:           r.Body,
		ResponseBody:          captureBody(respBody),
		HTTPMethod:            r.Method,
		HTTPURL:               r.URL,
		HTTPStatus:            status,
		StartedAt:             r.At,
		EndedAt:               at,
	}
}

// ForGate renders the request half as a Captured for the gate's evaluation,
// whose verdict must be obtained before a response exists.
//
// Note what changed underneath it: RequestBody is now the SELECTED document, so
// the gate -- and core's alignment judge downstream of it -- evaluates the newest
// turns rather than the head window of boilerplate they used to see. That is the
// point of selecting, and it is strictly better input, but it is a change to
// enforcement INPUT and not only to stored evidence. No shipped policy matches
// model-call content today (the seeded pack matches `command`, `file_path` and
// `file_operation`, none of which a relayed call carries), so nothing changes
// verdict today.
func (r RequestCapture) ForGate() Captured {
	return Captured{
		CredentialFingerprint: r.Fingerprint,
		RequestHeaders:        r.Headers,
		RequestBody:           r.Body,
		HTTPMethod:            r.Method,
		HTTPURL:               r.URL,
		StartedAt:             r.At,
	}
}

func stripQuery(url string) string {
	if i := strings.IndexAny(url, "?#"); i >= 0 {
		return url[:i]
	}
	return url
}
