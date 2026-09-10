package telemetryemit

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
)

// TestAttrValueBoundExceedsTheWireCap pins a relation that spans two modules
// and was, until this test, only a comment. Telemetry's collection bound must
// stay at or above the client's wire cap, or content arriving as an attribute
// is truncated by the receiver before capBody can act.
func TestAttrValueBoundExceedsTheWireCap(t *testing.T) {
	wireCapRunes := measureWireCapRunes(t)

	// Equality is acceptable; the cap can still act at exactly its limit; but
	// anything below it is the defect.
	if min := 4 * wireCapRunes; telemetry.MaxAttrValueBytes < min {
		t.Errorf("telemetry.MaxAttrValueBytes = %d, need >= %d (4 x the %d-rune wire cap). Below this, attribute-carried content truncates before capBody and the cap's drill goes vacuous.",
			telemetry.MaxAttrValueBytes, min, wireCapRunes)
	}
}

// measureWireCapRunes finds the client's content cap by emitting an oversized
// ASCII body and counting what arrived.
func measureWireCapRunes(t *testing.T) int {
	t.Helper()
	const oversize = 300000
	ev := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventType:     client.EventTurnCompleted,
		SessionID:     "sess-cap",
		DeveloperDID:  testDID,
		EventID:       "cap-probe",
		Timestamp:     "2026-08-28T10:00:00Z",
		Tool:          client.Tool{Name: "claude-code", Kind: client.ToolShell},
		TurnIndex:     new(int),
		Content:       &client.Content{Thinking: strings.Repeat("a", oversize)},
	}
	body := emitThrough(t, true, ev)

	var p struct {
		ActivityOutput struct {
			Thinking string `json:"thinking"`
		} `json:"activity_output"`
	}
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := utf8.RuneCountInString(p.ActivityOutput.Thinking)
	if got == 0 {
		t.Fatal("no content on the wire; the probe cannot measure the cap it needs")
	}
	if got >= oversize {
		t.Fatalf("content was NOT capped (%d runes of %d); the cap is gone, which is a defect in itself", got, oversize)
	}
	return got
}

// TestIdentifierBoundTruncatesOnARuneBoundary: a 300 KB identifier value must
// be bounded to 256 bytes without splitting a multi-byte UTF-8 rune in half.
// Unlike claude-code's capStr, which counts RUNES, this bound counts BYTES,
// because query_source arrives from a local network listener and
// telemetry.MaxAttrValueBytes alone would admit far more than an identifier
// needs.
func TestIdentifierBoundTruncatesOnARuneBoundary(t *testing.T) {
	// U+65E5 ("日") is 3 bytes wide. 256 is not a multiple of 3 (85*3 =
	// 255), so a naive byte[:256] slice lands mid-rune.
	const rune3 = "\u65e5"
	hostile := strings.Repeat(rune3, 100000) // 300,000 bytes
	if len(hostile) != 300000 {
		t.Fatalf("test setup: hostile is %d bytes, want 300000", len(hostile))
	}

	got := capIdentifier(hostile)

	if len(got) > 256 {
		t.Fatalf("capIdentifier returned %d bytes, want <= 256", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("capIdentifier split a rune: result is not valid UTF-8: %q", got)
	}
	if !strings.HasPrefix(hostile, got) {
		t.Error("capIdentifier did not return a genuine prefix of the input")
	}
	if want := 85 * len(rune3); len(got) != want {
		t.Errorf("capIdentifier returned %d bytes, want exactly %d (85 whole runes; the 256th byte starts an incomplete 86th rune and must be dropped, not kept dangling)", len(got), want)
	}
}

// TestIdentifierBoundLeavesShortValuesUntouched: the common case (a short
// vendor literal like "sdk" or a UUID) must not be reformatted or copied
// through the UTF-8 repair path at all.
func TestIdentifierBoundLeavesShortValuesUntouched(t *testing.T) {
	for _, s := range []string{"", "sdk", "agent:custom", strings.Repeat("a", 256)} {
		if got := capIdentifier(s); got != s {
			t.Errorf("capIdentifier(%q) = %q, want unchanged (%d bytes is at or under the bound)", s, got, len(s))
		}
	}
}
