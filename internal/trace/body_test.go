package trace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBodyUnderCapReturnsVerbatim(t *testing.T) {
	s := "hello world"
	got := Body(s)
	if got != s {
		t.Fatalf("Body(%q) = %#v, want the string unchanged", s, got)
	}
}

func TestBodyAtCapReturnsVerbatim(t *testing.T) {
	s := strings.Repeat("a", MaxBodyBytes)
	got := Body(s)
	if got != s {
		t.Fatalf("Body at exactly MaxBodyBytes should pass through unchanged")
	}
}

func TestBodyOverCapReturnsTruncatedMarker(t *testing.T) {
	// ASCII payload so byte length == rune count, isolating the truncation
	// math from UTF-8 boundary handling (covered separately below).
	s := strings.Repeat("b", MaxBodyBytes+100)
	got := Body(s)

	marker, ok := got.(truncatedMarker)
	if !ok {
		t.Fatalf("Body over cap = %#v (%T), want truncatedMarker", got, got)
	}
	if !marker.Truncated {
		t.Fatalf("Truncated = false, want true")
	}
	if marker.Bytes != len(s) {
		t.Fatalf("Bytes = %d, want %d", marker.Bytes, len(s))
	}

	wantSum := sha256.Sum256([]byte(s))
	if marker.SHA256 != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("SHA256 mismatch: got %s", marker.SHA256)
	}
	if len(marker.Head) != MaxBodyBytes {
		t.Fatalf("Head length = %d, want %d (ASCII payload, no boundary trim needed)", len(marker.Head), MaxBodyBytes)
	}
	if marker.Head != s[:MaxBodyBytes] {
		t.Fatalf("Head does not match expected prefix")
	}
}

func TestBodyOverCapHeadIsUTF8Safe(t *testing.T) {
	// Build a string whose byte MaxBodyBytes falls in the middle of a
	// multi-byte rune: pad with ASCII to MaxBodyBytes-1, then a 3-byte rune
	// (e.g. '€' U+20AC), so a naive s[:MaxBodyBytes] slice would split it.
	pad := strings.Repeat("a", MaxBodyBytes-1)
	s := pad + "€€€" // slicing at MaxBodyBytes lands 1 byte into the first €

	got := Body(s)
	marker, ok := got.(truncatedMarker)
	if !ok {
		t.Fatalf("Body over cap = %#v, want truncatedMarker", got)
	}
	if !utf8.ValidString(marker.Head) {
		t.Fatalf("Head is not valid UTF-8: %q", marker.Head)
	}
	if len(marker.Head) >= MaxBodyBytes {
		t.Fatalf("Head length = %d, want < %d (trimmed back from the split rune)", len(marker.Head), MaxBodyBytes)
	}
	if marker.Head != pad {
		t.Fatalf("Head = %q, want exactly the ASCII pad with the split rune dropped", marker.Head)
	}
	if marker.Bytes != len(s) {
		t.Fatalf("Bytes = %d, want %d", marker.Bytes, len(s))
	}
}

func TestJSONSmallValueIsRawMessage(t *testing.T) {
	v := map[string]any{"a": 1, "b": "two"}
	got := JSON(v)
	raw, ok := got.(json.RawMessage)
	if !ok {
		t.Fatalf("JSON(small) = %#v (%T), want json.RawMessage", got, got)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal raw message: %v", err)
	}
	if back["b"] != "two" {
		t.Fatalf("round-tripped value wrong: %+v", back)
	}
}

func TestJSONLargeValueIsTruncatedMarker(t *testing.T) {
	v := map[string]any{"payload": strings.Repeat("z", MaxBodyBytes+1)}
	got := JSON(v)
	marker, ok := got.(truncatedMarker)
	if !ok {
		t.Fatalf("JSON(large) = %#v (%T), want truncatedMarker", got, got)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal reference value: %v", err)
	}
	wantSum := sha256.Sum256(b)
	if marker.SHA256 != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("SHA256 mismatch: computed over marshalled JSON string, not v")
	}
	if marker.Bytes != len(b) {
		t.Fatalf("Bytes = %d, want %d", marker.Bytes, len(b))
	}
}

func TestBodyMarkerMarshalsAsPlainObject(t *testing.T) {
	// Record.Detail is map[string]any; Body's result must serialize as a
	// plain JSON object (not wrapped/typed) when embedded there.
	s := strings.Repeat("c", MaxBodyBytes+1)
	rec := Record{Stage: StageLog, Detail: map[string]any{"body": Body(s)}}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	var back struct {
		Detail struct {
			Body struct {
				Truncated bool   `json:"truncated"`
				Bytes     int    `json:"bytes"`
				SHA256    string `json:"sha256"`
				Head      string `json:"head"`
			} `json:"body"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal record: %v", err)
	}
	if !back.Detail.Body.Truncated || back.Detail.Body.Bytes != len(s) {
		t.Fatalf("round-tripped marker wrong: %+v", back.Detail.Body)
	}
}
