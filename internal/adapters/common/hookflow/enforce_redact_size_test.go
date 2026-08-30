package hookflow

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// TestRedactTextScansOversizedBodies a body over MaxRedactBody used to be
// returned unscanned, on the file path's skip-not-truncate rule.
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
		t.Error("an oversized body egressed unscanned; the first 64K of it is exactly " +
			"what capBody then puts on the wire")
	}
	if !strings.Contains(out, "OPENBOX_REDACTED") {
		t.Error("no redaction placeholder; the body was not scanned at all")
	}

	if !utf8.ValidString(out) {
		t.Error("truncation produced invalid UTF-8")
	}
}
