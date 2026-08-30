package telemetryemit

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
)

// TestAttrValueBoundExceedsTheWireCap pins a relation that spans two modules and
// was, until this test, only a comment.
//
// telemetry's collection bound must stay at or above the client's wire cap, or
// content arriving as an ATTRIBUTE is truncated by the receiver before capBody
// can act. Two consequences, and the second is the one that hides: real content
// would truncate tighter than OD1(c) blesses, silently; and the cap's own
// mutation drill would only ever exercise states the receiver cannot produce, so
// deleting the cap would leave the drill green. That is exactly what
// TestThinkingCollectionBoundExceedsTheWireCap forbids one lane over.
//
// The wire cap is MEASURED, not copied. client.maxBodySize is unexported, and a
// hand-copied number is the drift this test exists to catch.
func TestAttrValueBoundExceedsTheWireCap(t *testing.T) {
	wireCapRunes := measureWireCapRunes(t)

	// Worst case for UTF-8 is 4 bytes per rune. Equality is acceptable — the cap
	// can still act at exactly its limit — but anything below it is the defect.
	if min := 4 * wireCapRunes; telemetry.MaxAttrValueBytes < min {
		t.Errorf("telemetry.MaxAttrValueBytes = %d, need >= %d (4 x the %d-rune wire cap). Below this, attribute-carried content truncates before capBody and the cap's drill goes vacuous.",
			telemetry.MaxAttrValueBytes, min, wireCapRunes)
	}
}

// measureWireCapRunes finds the client's content cap by emitting an oversized
// ASCII body and counting what arrived.
//
// ASCII deliberately: at 4 bytes per rune a maximal value exactly equals the
// collection bound, so a multi-byte fixture cannot distinguish "capBody
// truncated it" from "the collection bound truncated it" — the very confusion
// this relation guards against.
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
		t.Fatal("no content on the wire — the probe cannot measure the cap it needs")
	}
	if got >= oversize {
		t.Fatalf("content was NOT capped (%d runes of %d) — the cap is gone, which is a defect in itself", got, oversize)
	}
	return got
}
