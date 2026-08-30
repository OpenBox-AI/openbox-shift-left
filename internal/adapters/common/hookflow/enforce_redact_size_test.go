package hookflow

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// A body over MaxRedactBody used to be returned UNSCANNED, on the file path's
// skip-not-truncate rule. That rule does not transfer here: every caller of
// RedactText attaches to a telemetry event, where nothing is replayed into the
// developer's machine, and the client then caps at 65536 runes — at most 256 KiB,
// strictly under this 512 KiB scan cap. So the skip did not protect anything; it
// just shipped the first 64K of a large body unscanned. A large `cat`, an install
// log or a build log reaches that size routinely.
//
// This asserts the direction rather than the mechanism: a secret near the START
// of an oversized body must be redacted, because the start is the part that
// actually egresses.
func TestRedactTextScansOversizedBodies(t *testing.T) {
	r := decision.NewRedactor()
	secret := "AKIA" + "IOSFODNN7EXAMPLE" // assembled: a repo scanner rewrites whole keys in source

	body := "AWS_ACCESS_KEY_ID=" + secret + "\n" + strings.Repeat("filler line\n", 60000)
	if len(body) <= MaxRedactBody {
		t.Fatalf("fixture is only %d bytes; it must exceed MaxRedactBody (%d) or the case "+
			"proves nothing", len(body), MaxRedactBody)
	}

	out := RedactText(r, body)
	if strings.Contains(out, secret) {
		t.Error("an oversized body egressed unscanned — the first 64K of it is exactly " +
			"what capBody then puts on the wire")
	}
	if !strings.Contains(out, "OPENBOX_REDACTED") {
		t.Error("no redaction placeholder; the body was not scanned at all")
	}

	// Truncation must be to a rune boundary: a split rune would reach the wire as
	// U+FFFD and can corrupt a JSON body the caller is carrying.
	if !utf8.ValidString(out) {
		t.Error("truncation produced invalid UTF-8")
	}
}
