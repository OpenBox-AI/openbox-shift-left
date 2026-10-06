package gateway

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// TestALargeReplyIsCutWithAMarkerSayingSo is the response half of the honesty
// property the request half already had.
//
// The cut lands the body at exactly captureBodyRunes, and capModelCallBody marks
// only a body STRICTLY longer than that same number, so an ASCII reply cut here
// passed the downstream check untouched: 65,536 bytes of a 307,200-byte reply,
// ending mid-frame, reading as a complete answer.
func TestALargeReplyIsCutWithAMarkerSayingSo(t *testing.T) {
	plain := strings.Repeat("a", 300*1024)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(plain)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	h := http.Header{}
	h.Set("Content-Encoding", "gzip")
	stored := captureBody(capturableBody(buf.Bytes(), h))

	if !strings.HasSuffix(stored, bodyCutNote) {
		t.Errorf("a cut reply carries no note; it ends %q", stored[max(0, len(stored)-60):])
	}
	// The downstream cap must not then take the note off again, which is what
	// makes the note reach the store rather than merely exist here.
	if got := client.MaxModelCallBodyBytes; len(stored) > got {
		t.Errorf("stored %d bytes, over the client's %d-byte model-call cap", len(stored), got)
	}
}

// TestAnUncutReplyCarriesNoNote the control: a note on a whole body is the same
// defect as no note on a cut one.
func TestAnUncutReplyCarriesNoNote(t *testing.T) {
	if got := captureBody("a short reply"); strings.Contains(got, "truncated here") {
		t.Errorf("a whole body claims a cut: %q", got)
	}
}

// TestTwoContentEncodingHeaderLinesAreRefused a doubly-encoded body must say so
// rather than decode one layer and store the rest as plaintext.
//
// `Get` returns only the first value, and isToken refuses only a comma-joined
// list inside ONE value, so two header lines -- the same list semantically --
// reduced to their first token: the relay gunzipped cleanly and returned raw
// brotli, which the keyword-driven redactor cannot see into.
func TestTwoContentEncodingHeaderLinesAreRefused(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(sentinelBody())); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	h := http.Header{}
	h.Add("Content-Encoding", "gzip")
	h.Add("Content-Encoding", "br")

	if got := capturableBody(buf.Bytes(), h); !strings.HasPrefix(got, markerPrefix) {
		t.Errorf("two encoding layers decoded to %q with no marker", got[:min(120, len(got))])
	}
	// One line still decodes, or the fix would have disabled capture entirely.
	single := http.Header{}
	single.Set("Content-Encoding", "gzip")
	if got := capturableBody(buf.Bytes(), single); !strings.Contains(got, decodedSentinel) {
		t.Errorf("a single gzip layer no longer decodes: %q", got)
	}
}

// decodedSentinel is plain and keyword-free on purpose: a credential-shaped
// fixture is rewritten in place by this repo's own redactor before it ever runs.
const decodedSentinel = "the-decoded-sentinel"

func sentinelBody() []byte { return []byte(`{"reply":"` + decodedSentinel + `"}`) }

// TestAnUnboundedDroppedKeyListCannotEvictTheConversation dropped_keys sizes the
// skeleton, so without a bound it drives `room` negative and no message fits --
// the failure modelBudget exists to prevent, through a field the selector itself
// introduced. Measured before the bound: 49,152 stored bytes of one key name,
// the turn absent, under a marker blaming an oversized message.
func TestAnUnboundedDroppedKeyListCannotEvictTheConversation(t *testing.T) {
	const turn = "WHAT-IS-MY-NEWEST-TURN"
	cases := map[string]string{
		"one enormous key name": `{"messages":[{"role":"user","content":"` + turn + `"}],"` +
			strings.Repeat("K", 60*1024) + `":1}`,
		"many ordinary keys": manyKeyBody(turn, 4000),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			out := selectModelCallRequest(body)
			if !strings.Contains(out, turn) {
				t.Errorf("the newest turn was evicted; the document starts %q", out[:min(160, len(out))])
			}
			if len(out) > selectionBudget {
				t.Errorf("selected %d bytes, over the %d-byte budget", len(out), selectionBudget)
			}
		})
	}
}

// manyKeyBody builds a body with n distinct top-level keys besides `messages`.
// The key names are DERIVED rather than written out: a literal alphabet reads as
// a high-entropy value to this repo's own redactor, which rewrites developer
// files, and it silently shortened an earlier version of this helper.
func manyKeyBody(turn string, n int) string {
	var b strings.Builder
	b.WriteString(`{"messages":[{"role":"user","content":"` + turn + `"}]`)
	for i := range n {
		b.WriteString(`,"k` + strings.Repeat("x", 26) + strconv.Itoa(i) + `":1`)
	}
	b.WriteString("}")
	return b.String()
}

// TestAnOverlongMarkerKeepsBothOfItsEnds a marker is one claim, and half a claim
// is worse than a short one.
//
// The old path handed the whole marker to markedTailWindow as its marker with an
// empty payload, which head-cut it: the closing bracket and the explanation went,
// which is verbatim the symptom rewindowMarkedBody was written to remove.
func TestAnOverlongMarkerKeepsBothOfItsEnds(t *testing.T) {
	marker := markerPrefix + "not captured; the body used Content-Encoding " +
		strings.Repeat("E", 90*1024) + ", which this relay cannot decode]"

	out := rewindowMarkedBody(marker)
	if len(out) > selectionBudget {
		t.Errorf("re-windowed to %d bytes, over the %d-byte budget", len(out), selectionBudget)
	}
	if !strings.HasPrefix(out, markerPrefix) {
		t.Error("the marker's own prefix did not survive")
	}
	if !strings.HasSuffix(out, "]") {
		t.Errorf("the closing bracket went, so the claim is unterminated; it ends %q", out[max(0, len(out)-24):])
	}
}

// TestAWholeBodyIsNotStoredBehindAWindowClaim the marker wording follows what
// actually happened, because this string is the GATE's input as well as stored
// evidence: ForGate hands it to Decide.
func TestAWholeBodyIsNotStoredBehindAWindowClaim(t *testing.T) {
	body := `{"organization":"acme","limit":25}`
	out := selectModelCallRequest(body)

	if !strings.HasSuffix(out, body) {
		t.Fatalf("the body was cut when it fits whole: %q", out)
	}
	if strings.Contains(out, "fell back to a tail window") {
		t.Errorf("a whole body is stored behind a claim that a window was cut: %q", out)
	}
	if !strings.HasPrefix(out, markerPrefix) {
		t.Error("selection still fell back, so it must still say why")
	}
}
