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
	return capRunes(clampAndRedact(body))
}

// clampAndRedact is the prefix both funnels share: bound what the redactor is
// asked to scan, keep that cut on a rune boundary, then redact. captureRequestBody
// documents itself as differing from captureBody "by one step", and sharing this is
// what makes that claim structural rather than a convention two copies can drift
// out of -- the bounds tests only ever exercised one of the copies.
func clampAndRedact(body string) string {
	if len(body) > maxCaptureInputBytes {
		body = body[:maxCaptureInputBytes]
	}
	redacted, _, _ := bodyRedactor.RedactText(trimPartialRune(body))
	return redacted
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
// than merely unlikely -- but only once the marked case is handled too, because
// the selector returns a marked body unchanged and redaction can have grown that
// one past the budget as well. rewindowMarkedBody is what closes it; without it,
// 4 measured rows reached the store at 49,164-49,232 bytes.
func captureRequestBody(body string) string {
	if body == "" {
		return ""
	}
	redacted := clampAndRedact(body)
	if len(redacted) > selectionBudget {
		redacted = selectModelCallRequest(redacted)
		// A MARKED body comes back from there untouched, so it can still be over
		// the budget -- and the only bound left would be capRunes, which head-cuts
		// and would take the end off a tail window. Re-window it, marker intact.
		if len(redacted) > selectionBudget {
			redacted = rewindowMarkedBody(redacted)
		}
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

// bodyCutNote marks a truncated captured body, at the END: a reply is a head
// window, and markerPrefix at position 0 would make selectModelCallRequest read
// it as a claim about the whole body.
//
// Unmarked, the cut was invisible twice over. capRunes lands the result at
// exactly captureBodyRunes, and capModelCallBody marks only a body STRICTLY
// longer than that same number, so an ASCII reply cut here passed untouched:
// measured, 65,536 bytes of a 307,200-byte reply, cut mid-frame, claiming to be
// whole. That is the mode the request direction grew fallbackWindow to remove.
const bodyCutNote = "\n[openbox: truncated here; the rest of this body is not stored]"

// noteCut appends bodyCutNote within a rune budget, so the note cannot push the
// result back over the cap that produced it.
func noteCut(r []rune, budget int) string {
	keep := budget - utf8.RuneCountInString(bodyCutNote)
	if keep < 0 {
		keep = 0
	}
	if keep > len(r) {
		keep = len(r)
	}
	return string(r[:keep]) + bodyCutNote
}

func capRunes(s string) string {
	if len(s) <= captureBodyRunes { // byte length ≤ cap ⇒ rune count ≤ cap
		return s
	}
	r := []rune(s)
	if len(r) <= captureBodyRunes {
		return s
	}
	return noteCut(r, captureBodyRunes)
}

// Captured is the evidence one relayed model call produces.
type Captured struct {
	RequestHeaders  map[string]string
	ResponseHeaders map[string]string
	RequestBody     string
	ResponseBody    string

	// ResponseBytesSeen and ResponseTruncated are the response-side twin of
	// selectionNote (requestselect.go): the gateway's own view of whether the
	// stored ResponseBody is the whole reply. Set by the emit site (proxy.go),
	// after Complete returns -- not by Complete itself, which only ever sees
	// the already-computed ResponseBody string and the request half's fields,
	// never the sink that observed the cut. Both stay at their zero value on
	// a path that never ran a response through a sink (a refusal or an
	// unreachable upstream, neither of which carries a ResponseBody either).
	ResponseBytesSeen int
	ResponseTruncated bool

	CredentialFingerprint string
	HTTPMethod            string
	HTTPURL               string
	HTTPStatus            int

	// Attribution is a provider-specific read of the RAW request bytes (an
	// injected closure; see Gateway.WithRequestAttribution), carried without
	// this package knowing what the keys mean. Nil when no such reader is
	// configured, or when the configured one found nothing.
	Attribution map[string]string

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

	// Attribution is assigned AFTER construction by the caller (proxy.go),
	// never through CaptureRequest's signature: that constructor is exported
	// and already called from tests, and adding a parameter here would break
	// every one of them for a feature most callers do not need.
	Attribution map[string]string
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
		Attribution:           r.Attribution,
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
		Attribution:           r.Attribution,
		StartedAt:             r.At,
	}
}

// RawCapture is one relayed call's evidence BEFORE clampAndRedact touches its
// bodies -- request/response headers still have credentialHeaders scrubbed
// (redactHeaders), because a header value is a bearer token or a session
// cookie outright and never belongs in any observer's hands, raw or not, but
// RequestBody/ResponseBody are exactly what was read off the wire. It exists
// so a caller wanting full-fidelity local capture (see WithRawObserver) never
// has to reach past the redaction step that produces the client-facing
// Captured value -- this package still redacts every body it hands to an
// Emitter; RawCapture is an ADDITIONAL, earlier view, not a replacement.
type RawCapture struct {
	Method          string
	URL             string
	Status          int
	RequestHeaders  map[string]string
	ResponseHeaders map[string]string
	RequestBody     string
	ResponseBody    string
	StartedAt       time.Time
	EndedAt         time.Time
}

func stripQuery(url string) string {
	if i := strings.IndexAny(url, "?#"); i >= 0 {
		return url[:i]
	}
	return url
}
