package gateway

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"strings"
)

// encodingGzip is the whole decode set, and that is a measurement: gzip was on 118
// of 118 captured responses, which is why every response body was a placeholder.
// `br` and `zstd` are unobserved and would cost a dependency.
const encodingGzip = "gzip"

const encodingGzipLegacy = "x-gzip"

// isGzip reads Content-Encoding as the LIST it is; two layers stay refused.
func isGzip(encoding string) bool {
	tokens := strings.Split(encoding, ",")
	if len(tokens) > 1 {
		return false
	}
	only := strings.TrimSpace(tokens[0])
	return strings.EqualFold(only, encodingGzip) || strings.EqualFold(only, encodingGzipLegacy)
}

// decodeCapturable decompresses the teed copy, capture path only, bounding the
// DECOMPRESSED stream and stopping AT the bound.
func decodeCapturable(body []byte, encoding string) string {
	if !isGzip(encoding) {
		return fmt.Sprintf("[openbox: not captured; the body used Content-Encoding %q, which this "+
			"relay cannot decode, so redaction could not inspect it]", encoding)
	}
	// A zero-byte body is not a fault: a 204 or an aborted call produces one.
	if len(body) == 0 {
		return ""
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return undecodableMarker(encoding)
	}
	defer zr.Close()

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
