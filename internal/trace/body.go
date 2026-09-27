package trace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"unicode/utf8"
)

// truncatedMarker is what Body/JSON stores in place of a body over
// MaxBodyBytes: a reader can still recognize the body by its hash and length
// and can still see its shape via head, without the trace itself carrying an
// unbounded blob.
type truncatedMarker struct {
	Truncated bool   `json:"truncated"`
	Bytes     int    `json:"bytes"`
	SHA256    string `json:"sha256"`
	Head      string `json:"head"`
}

// Body returns s verbatim when it fits under MaxBodyBytes, else a marker
// carrying the full-length sha256, the original byte count, and a
// UTF-8-safe head (the first MaxBodyBytes bytes, trimmed back to a full rune
// boundary so the marker is itself valid JSON string data rather than a cut
// multi-byte sequence).
func Body(s string) any {
	if len(s) <= MaxBodyBytes {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return truncatedMarker{
		Truncated: true,
		Bytes:     len(s),
		SHA256:    hex.EncodeToString(sum[:]),
		Head:      utf8Head(s, MaxBodyBytes),
	}
}

// utf8Head returns the first n bytes of s, trimmed back so it never ends on
// a truncated multi-byte rune. s is assumed already valid UTF-8 (Go strings
// from JSON/text sources are), so the only way the prefix can be invalid is
// a cut at the tail.
func utf8Head(s string, n int) string {
	if n >= len(s) {
		return s
	}
	head := s[:n]
	for len(head) > 0 && !utf8.ValidString(head) {
		head = head[:len(head)-1]
	}
	return head
}

// JSON marshals v and returns it as json.RawMessage when the encoding fits
// under MaxBodyBytes, else the same truncated marker Body produces, computed
// over the marshalled JSON string rather than v itself. A marshal failure is
// folded into the marker path too (over err.Error()) so a bad Detail value
// degrades a record instead of losing the Emit that carries it.
func JSON(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return Body("trace: JSON marshal failed: " + err.Error())
	}
	if len(b) <= MaxBodyBytes {
		return json.RawMessage(b)
	}
	return Body(string(b))
}
