package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
)

// gzipOf is the provider's shape: Cloudflare compresses every response, so a
// content-encoded body is the rule rather than the documented edge case the
// placeholder was written for.
func gzipOf(t *testing.T, plain string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(plain)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// encodedUpstream answers with a fixed body under a fixed Content-Encoding.
func encodedUpstream(t *testing.T, encoding string, body []byte) *memhttptest.Server {
	t.Helper()
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", encoding)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestGzippedResponseRelaysCompressedAndCapturesDecoded is the phase's
// load-bearing test, and it asserts both halves of the trade the phase makes:
// the client still receives the provider's exact compressed bytes, and the
// capture holds text a reader (and the redactor) can inspect.
func TestGzippedResponseRelaysCompressedAndCapturesDecoded(t *testing.T) {
	const plain = `{"type":"message","content":[{"type":"text","text":"hello"}]}`
	compressed := gzipOf(t, plain)
	upstream := encodedUpstream(t, "gzip", compressed)

	em := &recordingEmitter{}
	srv := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

	resp, err := probeClient().Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	relayed, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("reading the relayed body: %v", err)
	}

	if !bytes.Equal(relayed, compressed) {
		t.Errorf("the relay altered the forwarded bytes: got %d bytes, want the provider's %d", len(relayed), len(compressed))
	}
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding reaching the client = %q, want gzip; the client must decode what it was told it got", got)
	}

	got := em.await(t, 1)[0].ResponseBody
	if got != plain {
		t.Errorf("captured response body = %q, want the decompressed %q", got, plain)
	}
}

// TestGzippedRequestBodyIsCapturedDecoded the request direction runs through
// the same funnel, so a client that ever compresses upward gets the same
// treatment rather than a second implementation's.
func TestGzippedRequestBodyIsCapturedDecoded(t *testing.T) {
	const plain = `{"model":"claude-opus-4-8","messages":[]}`
	var got recorded
	upstream := upstreamRecorder(t, &got, nil)

	em := &recordingEmitter{}
	srv := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

	compressed := gzipOf(t, plain)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", bytes.NewReader(compressed))
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()

	if !bytes.Equal(got.body, compressed) {
		t.Error("the relay altered the forwarded request body")
	}
	if body := em.await(t, 1)[0].RequestBody; body != plain {
		t.Errorf("captured request body = %q, want the decompressed %q", body, plain)
	}
}

// TestCompressedBodyIsBoundedByDecompressedBytes is the memory bound, and it is
// asserted in bytes because that is the unit the bound claims. Without it a
// small compressed body expands without limit.
func TestCompressedBodyIsBoundedByDecompressedBytes(t *testing.T) {
	// Highly compressible, and far past the input bound once expanded.
	plain := strings.Repeat("A", 8*maxCaptureInputBytes)
	compressed := gzipOf(t, plain)
	if len(compressed) >= maxCaptureInputBytes {
		t.Fatalf("fixture is not compressible enough to test the bound: %d compressed bytes", len(compressed))
	}
	upstream := encodedUpstream(t, "gzip", compressed)

	em := &recordingEmitter{}
	srv := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

	resp, err := probeClient().Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()

	got := em.await(t, 1)[0].ResponseBody
	if len(got) > captureBodyRunes {
		t.Errorf("captured %d bytes; the wire cap is %d runes and every rune here is one byte", len(got), captureBodyRunes)
	}
	if got == "" {
		t.Error("an over-long compressed body captured nothing; the bound must truncate, never drop")
	}
	if strings.Contains(got, "openbox:") {
		t.Errorf("an over-long body was reported as a decode fault: %q", got[:min(len(got), 200)])
	}
}

// TestUnknownContentEncodingYieldsAMarkerNamingIt an encoding the relay cannot
// decode must say which one, or the next investigation cannot tell an
// unsupported codec from a corrupt stream.
// `br` is deliberately NOT in this list any more: it is the modal response
// encoding, and marking it was the defect. `zstd` and `deflate` stay -- the
// corpus client advertises both and no origin has ever returned either, so an
// honest marker beats a dependency this relay has not earned.
func TestUnknownContentEncodingYieldsAMarkerNamingIt(t *testing.T) {
	for _, encoding := range []string{"zstd", "deflate", "gzip, br"} {
		t.Run(encoding, func(t *testing.T) {
			upstream := encodedUpstream(t, encoding, []byte("whatever these bytes are"))

			em := &recordingEmitter{}
			srv := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

			resp, err := probeClient().Get(srv.URL + "/v1/messages")
			if err != nil {
				t.Fatalf("request through gateway: %v", err)
			}
			resp.Body.Close()

			got := em.await(t, 1)[0].ResponseBody
			if !strings.Contains(got, "openbox:") {
				t.Errorf("an undecodable body was captured as bytes rather than marked: %q", got)
			}
			if !strings.Contains(got, encoding) {
				t.Errorf("the marker does not name the encoding %q: %q", encoding, got)
			}
		})
	}
}

// TestIdentityEncodingIsUnaffected identity is not a codec, and treating it as
// one would put a marker where a body belongs.
func TestIdentityEncodingIsUnaffected(t *testing.T) {
	const plain = `{"ok":true}`
	upstream := encodedUpstream(t, "identity", []byte(plain))

	em := &recordingEmitter{}
	srv := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

	resp, err := probeClient().Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()

	if got := em.await(t, 1)[0].ResponseBody; got != plain {
		t.Errorf("captured %q, want %q", got, plain)
	}
}

// TestTruncatedGzipStreamCapturesWhatDecoded a turn the developer aborted ends
// mid-frame, which is normal. It must read as a short body, not as a decode
// fault, or every cancelled turn looks like a relay bug.
func TestTruncatedGzipStreamCapturesWhatDecoded(t *testing.T) {
	const plain = `event: message_start
data: {"type":"message_start"}

event: content_block_delta
data: {"delta":{"text":"partial answer here"}}

`
	full := gzipOf(t, plain)
	// Cut the trailer and some of the final block: the member never completes.
	cut := full[:len(full)-16]
	upstream := encodedUpstream(t, "gzip", cut)

	em := &recordingEmitter{}
	srv := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

	resp, err := probeClient().Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()

	got := em.await(t, 1)[0].ResponseBody
	if strings.Contains(got, "openbox:") {
		t.Errorf("an aborted stream was captured as a decode fault: %q", got)
	}
	if !strings.Contains(got, "message_start") {
		t.Errorf("the decoded prefix of an aborted stream was discarded: %q", got)
	}
}

// TestUndecodableGzipYieldsAMarker bytes that never were gzip are a genuine
// fault, and the honest answer is a marker rather than an empty body that reads
// as "the provider said nothing".
func TestUndecodableGzipYieldsAMarker(t *testing.T) {
	upstream := encodedUpstream(t, "gzip", []byte("this is not gzip at all"))

	em := &recordingEmitter{}
	srv := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

	resp, err := probeClient().Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()

	got := em.await(t, 1)[0].ResponseBody
	if !strings.Contains(got, "openbox:") || !strings.Contains(got, "gzip") {
		t.Errorf("a corrupt gzip body did not produce a gzip-naming marker: %q", got)
	}
}

// TestTheDecodeSetIsMatchedAsAHeaderToken: ${OPENBOX_REDACTED_SECRET_ASSIGNMENT} is a LIST header,
// and a whole-value compare made `x-gzip` -- a spelling origins still emit --
// store the undecodable marker for a body gzip.NewReader reads without
// complaint. That is the same shape as the defect this file exists for: a
// placeholder that reads as a documented limit rather than as a bug.
func TestTheDecodeSetIsMatchedAsAHeaderToken(t *testing.T) {
	for _, tc := range []struct {
		encoding string
		want     bool
	}{
		{"gzip", true},
		{"x-gzip", true},
		{"GZIP", true},
		{" gzip ", true},
		{"X-Gzip", true},
		{"br", true},
		{"BR", true},
		{" br ", true},
		{"zstd", false},
		{"deflate", false},
		// Two layers, one decoder: refusing is honest, claiming to have decoded is not.
		{"gzip, gzip", false},
		{"br, gzip", false},
	} {
		if got := isDecodable(tc.encoding); got != tc.want {
			t.Errorf("isDecodable(%q) = %v, want %v", tc.encoding, got, tc.want)
		}
	}
}

// TestALegacyGzipSpellingIsDecodedNotMarked closes the loop on the wire: the
// token test above would still pass if decodeCapturable ignored it.
func TestALegacyGzipSpellingIsDecodedNotMarked(t *testing.T) {
	const plain = `{"content":[{"text":"hello from x-gzip"}]}`
	got := decodeCapturable(gzipOf(t, plain), "x-gzip")
	if got != plain {
		t.Errorf("decodeCapturable(x-gzip) = %q, want the decoded body %q", got, plain)
	}
}

// brotliOf is the modal provider shape, not an edge case: across the recorded
// 8.4 GB corpus every one of 74,477 `application/json` replies came back
// brotli-encoded, with zero exceptions. The gzip fixtures above cover
// `text/event-stream` and nothing else.
func brotliOf(t *testing.T, plain string) []byte {
	t.Helper()
	var buf bytes.Buffer
	bw := brotli.NewWriter(&buf)
	if _, err := bw.Write([]byte(plain)); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := bw.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	return buf.Bytes()
}

// TestBrotliResponseRelaysCompressedAndCapturesDecoded is this phase's
// load-bearing test, and it is deliberately the wired one: decodeCapturable
// passing in isolation is what the gzip pair already proves. What was broken is
// the whole path -- 89.5% of stored response bodies were a marker naming `br` --
// so the assertion has to run through the relay, on both halves of the same
// trade the gzip test makes: the client still receives the provider's exact
// compressed bytes, and the capture holds text the redactor can inspect.
func TestBrotliResponseRelaysCompressedAndCapturesDecoded(t *testing.T) {
	const plain = `{"type":"message","content":[{"type":"text","text":"hello from brotli"}]}`
	compressed := brotliOf(t, plain)
	upstream := encodedUpstream(t, "br", compressed)

	em := &recordingEmitter{}
	srv := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

	resp, err := probeClient().Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	relayed, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("reading the relayed body: %v", err)
	}

	if !bytes.Equal(relayed, compressed) {
		t.Errorf("the relay altered the forwarded bytes: got %d bytes, want the provider's %d", len(relayed), len(compressed))
	}
	if got := resp.Header.Get("Content-Encoding"); got != "br" {
		t.Errorf("Content-Encoding reaching the client = %q, want br; the client must decode what it was told it got", got)
	}

	got := em.await(t, 1)[0].ResponseBody
	if got != plain {
		t.Errorf("captured response body = %q, want the decompressed %q", got, plain)
	}
}

// TestBrotliRequestBodyIsCapturedDecoded the request direction runs through the
// same funnel, so the two encodings cannot drift apart by direction.
func TestBrotliRequestBodyIsCapturedDecoded(t *testing.T) {
	const plain = `{"model":"claude-opus-5","messages":[]}`
	var got recorded
	upstream := upstreamRecorder(t, &got, nil)

	em := &recordingEmitter{}
	srv := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

	compressed := brotliOf(t, plain)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", bytes.NewReader(compressed))
	req.Header.Set("Content-Encoding", "br")
	req.Header.Set("Content-Type", "application/json")
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()

	if captured := em.await(t, 1)[0].RequestBody; captured != plain {
		t.Errorf("captured request body = %q, want the decompressed %q", captured, plain)
	}
}

// TestACorruptBrotliStreamIsMarkedNotStoredAsGarbage brotli.NewReader has no
// constructor error -- a corrupt stream only fails on the first Read -- so the
// gzip path's constructor check does not cover this codec. Without the
// empty-and-errored branch below a truncated body would store as "" and read as
// a 204.
func TestACorruptBrotliStreamIsMarkedNotStoredAsGarbage(t *testing.T) {
	got := decodeCapturable([]byte{0x1f, 0x8b, 0x08, 0x00, 0xde, 0xad, 0xbe, 0xef}, "br")
	if !strings.Contains(got, "openbox:") {
		t.Errorf("a corrupt br body was captured as bytes rather than marked: %q", got)
	}
	if !strings.Contains(got, "br") {
		t.Errorf("the marker does not name the encoding: %q", got)
	}
}
