package gateway

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"strings"

	"github.com/andybalholm/brotli"
)

// The decode set, and it is a measurement -- this time of the encoding VALUES,
// which is what the previous one was missing. Across a recorded 8.4 GB proxy
// corpus, 83,190 responses carried exactly two encodings with zero exceptions:
// `application/json` replies came back brotli (74,477; 89.5%) and
// `text/event-stream` replies gzip (7,378; 8.9%). A gzip-only decode set
// therefore stored a marker in place of 89.5% of every response body captured.
//
// The comment this replaces claimed gzip "was on 118 of 118 captured responses"
// and that br was "unobserved and would cost a dependency". All three halves
// were wrong: the cited measurement recorded only that Content-Encoding was
// PRESENT, never which one, so the claim was unfalsifiable; br was the modal
// encoding; and brotli was already in this binary's linked closure via
// internal/decision -> gitleaks -> mholt/archives. What it cost was a line in
// internal/gateway's depguard allowlist, which is the designed review point.
//
// `zstd` and `deflate` stay markers and are named in decodeSetExceptions: the
// corpus client advertises both and no origin returned either even once. That
// pairing is now enforced by TestTheDecodeSetCoversEveryEncodingTheCorpusAdvertises,
// so an advertised-but-undecoded encoding is a test failure rather than a
// comment nobody can check.
const (
	encodingGzip       = "gzip"
	encodingGzipLegacy = "x-gzip"
	encodingBrotli     = "br"
)

// newDecompressor returns the constructor for a Content-Encoding, or nil when
// this relay cannot decode it. Content-Encoding is read as the LIST it is; two
// layers stay refused, because storing bytes that are still compressed under a
// claim of having decoded them is worse than saying so.
func newDecompressor(encoding string) func(io.Reader) (io.Reader, error) {
	switch {
	case isToken(encoding, encodingGzip), isToken(encoding, encodingGzipLegacy):
		return func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) }
	case isToken(encoding, encodingBrotli):
		// brotli.NewReader has no constructor error, so a corrupt stream cannot
		// surface here the way a bad gzip header does; it lands on
		// decodeCapturable's empty-and-errored branch instead. That branch is
		// load-bearing for this codec, not belt-and-braces.
		return func(r io.Reader) (io.Reader, error) { return brotli.NewReader(r), nil }
	}
	return nil
}

// isDecodable answers whether the capture path can read a body under this
// Content-Encoding at all.
func isDecodable(encoding string) bool {
	return newDecompressor(encoding) != nil
}

// isToken compares against one header token rather than the whole value, which
// is what makes `x-gzip` -- a spelling origins still emit -- decode instead of
// storing a marker for a body gzip reads without complaint.
func isToken(encoding, want string) bool {
	tokens := strings.Split(encoding, ",")
	if len(tokens) > 1 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(tokens[0]), want)
}

// decodeCapturable decompresses the teed copy, capture path only, bounding the
// DECOMPRESSED stream and stopping AT the bound.
func decodeCapturable(body []byte, encoding string) string {
	newReader := newDecompressor(encoding)
	if newReader == nil {
		return fmt.Sprintf("[openbox: not captured; the body used Content-Encoding %q, which this "+
			"relay cannot decode, so redaction could not inspect it]", encoding)
	}
	// A zero-byte body is not a fault: a 204 or an aborted call produces one.
	if len(body) == 0 {
		return ""
	}
	zr, err := newReader(bytes.NewReader(body))
	if err != nil {
		return undecodableMarker(encoding)
	}
	if c, ok := zr.(io.Closer); ok {
		defer c.Close()
	}

	plain, err := io.ReadAll(io.LimitReader(zr, maxCaptureInputBytes))
	if len(plain) == 0 && err != nil {
		return undecodableMarker(encoding)
	}
	// A short read is not a fault: an aborted turn ends mid-frame, and the prefix
	// that decoded is the evidence.
	return string(plain)
}

func undecodableMarker(encoding string) string {
	return fmt.Sprintf("[openbox: not captured; the %s body could not be decoded (truncated "+
		"or corrupt), so redaction could not inspect it]", encoding)
}
