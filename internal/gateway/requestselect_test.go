package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// corpusShapedRequest reproduces the modal provider request from the recorded
// corpus: `model` first, `messages` SECOND, and `tools` trailing -- 65.5% of
// recorded bodies -- with a p50 size of 520,452 bytes. The key order matters to
// every assertion here, because the defect being fixed was a byte window over
// exactly this layout.
func corpusShapedRequest(t *testing.T, messages int, perMessage int, newest string) string {
	t.Helper()
	return corpusShapedRequestWithTools(t, messages, perMessage, newest, 60)
}

// corpusShapedRequestWithTools is the same shape with the trailing `tools` block
// sized explicitly. A real provider request carries tens of KB of tool schemas,
// and that size is what decides what a window over the WHOLE body keeps: once
// the trailing block alone fills the budget, such a window holds no conversation.
func corpusShapedRequestWithTools(t *testing.T, messages, perMessage int, newest string, toolCount int) string {
	t.Helper()
	msgs := make([]json.RawMessage, 0, messages)
	for i := range messages {
		text := strings.Repeat("h", perMessage)
		if i == messages-1 {
			text = newest
		}
		one, err := json.Marshal(map[string]any{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": text}},
		})
		if err != nil {
			t.Fatalf("marshal message %d: %v", i, err)
		}
		msgs = append(msgs, one)
	}
	// Assembled by hand rather than with json.Marshal(map), because a map would
	// sort the keys and destroy the very ordering this fixture exists to model.
	msgsJSON, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	tools := strings.Repeat(`{"name":"Bash","description":"run a shell command","input_schema":{"type":"object"}},`, toolCount)
	tools = "[" + strings.TrimSuffix(tools, ",") + "]"
	return fmt.Sprintf(`{"model":"claude-opus-5","messages":%s,"system":[{"type":"text","text":%q}],"tools":%s,"max_tokens":32000,"stream":true}`,
		msgsJSON, strings.Repeat("s", 8192), tools)
}

// TestTheNewestTurnSurvivesSelection is selection's load-bearing assertion and
// the one that fails on the pre-selection tree. Every byte window over a
// corpus-shaped body stored boilerplate: `messages` is p50 82.1% of the body and
// 95.3% of bodies exceed 64 KiB, so a head window keeps `model` plus the start of
// the history and a tail window keeps `tools`.
func TestTheNewestTurnSurvivesSelection(t *testing.T) {
	const newest = "THE_NEWEST_TURN_XYZZY"
	body := corpusShapedRequest(t, 200, 2600, newest)
	if len(body) < 400_000 {
		t.Fatalf("fixture is %d bytes; it must exceed the caps it exercises", len(body))
	}

	got := selectModelCallRequest(body)

	if !strings.Contains(got, newest) {
		t.Fatalf("the newest turn is absent from the selected body (%d bytes kept of %d)", len(got), len(body))
	}
	// The judge keeps head 3/5 + tail 2/5 of its window, so presence is not
	// enough: the newest turn has to be in the part that is kept.
	if at := strings.Index(got, newest); at < len(got)*3/5 {
		t.Errorf("the newest turn is at byte %d of %d -- inside the middle the alignment judge elides; "+
			"it must land in the last 2/5", at, len(got))
	}
}

// TestSelectionIsOneDistinctValuePerCall the measured defect was 28 calls storing
// 4 distinct bodies, because the stored window was the tail of the head and the
// head is the part that does not change between calls. Two calls differing only
// in their newest turn must now store different bodies.
func TestSelectionIsOneDistinctValuePerCall(t *testing.T) {
	first := selectModelCallRequest(corpusShapedRequest(t, 200, 2600, "TURN_ONE"))
	second := selectModelCallRequest(corpusShapedRequest(t, 200, 2600, "TURN_TWO"))

	if first == second {
		t.Error("two calls with different newest turns selected byte-identical bodies; " +
			"this is the defect -- the stored value is a constant, not evidence")
	}
	if !strings.Contains(first, "TURN_ONE") || !strings.Contains(second, "TURN_TWO") {
		t.Error("each selection must contain its own newest turn")
	}
}

// TestMessagesIsTheLastKeyInTheSelectedDocument is the trap selection exists to
// avoid, and it is not hypothetical: json.Marshal of a MAP sorts keys, which puts
// "messages" first -- ahead of "model", "system" and the selection note -- landing
// the newest turn in the blind middle of the judge's window. Only a struct with
// the field declared last survives this.
func TestMessagesIsTheLastKeyInTheSelectedDocument(t *testing.T) {
	got := selectModelCallRequest(corpusShapedRequest(t, 40, 2600, "NEWEST"))

	messagesAt := strings.Index(got, `"messages"`)
	if messagesAt < 0 {
		t.Fatalf("no messages key in the selected document: %q", got[:min(len(got), 300)])
	}
	for _, earlier := range []string{`"model"`, `"system"`, `"openbox_selection"`} {
		at := strings.Index(got, earlier)
		if at < 0 {
			t.Errorf("%s is absent from the selected document", earlier)
			continue
		}
		if at > messagesAt {
			t.Errorf("%s appears at byte %d, AFTER messages at %d; the field order is the whole "+
				"point -- a map's sorted keys would put messages first", earlier, at, messagesAt)
		}
	}
}

// TestSelectionDropsToolDefinitionsAndSaysSo `tools` is the boilerplate that was
// being stored 28 times over. Dropping it is also the one change here that
// REDUCES egress.
func TestSelectionDropsToolDefinitionsAndSaysSo(t *testing.T) {
	got := selectModelCallRequest(corpusShapedRequest(t, 40, 2600, "NEWEST"))

	if strings.Contains(got, "input_schema") {
		t.Error("tool definitions survived selection; they are the constant that made every stored body identical")
	}
	if !strings.Contains(got, `"tools"`) {
		t.Error("tools was dropped without being named in dropped_keys; an unrecorded drop is the " +
			"failure mode selection repairs, not a smaller version of it")
	}
	var doc struct {
		Selection struct {
			DroppedMessages int      `json:"dropped_messages"`
			DroppedKeys     []string `json:"dropped_keys"`
			OriginalBytes   int      `json:"original_bytes"`
		} `json:"openbox_selection"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("the selected document must stay valid JSON: %v", err)
	}
	if doc.Selection.OriginalBytes == 0 {
		t.Error("original_bytes is unset; the note must say what the selection was made from")
	}
	if doc.Selection.DroppedMessages == 0 {
		t.Error("dropped_messages is 0 for a body whose history cannot fit; the count must be real")
	}
}

// TestTheSelectedDocumentIsAlwaysValidJSON a consumer reading `content` gets
// something parseable, or the selection has replaced one unreadable artifact with
// another.
func TestTheSelectedDocumentIsAlwaysValidJSON(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"corpus shaped", corpusShapedRequest(t, 200, 2600, "NEWEST")},
		{"small and complete", `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`},
		{"one huge message", `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("q", 200_000) + `"}]}`},
		{"system is a string", `{"model":"m","system":"` + strings.Repeat("s", 40_000) + `","messages":[{"role":"user","content":"hi"}]}`},
		{"empty messages", `{"model":"m","messages":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := selectModelCallRequest(tc.body)
			if strings.HasPrefix(got, markerPrefix) {
				return // an honest fallback marker is not JSON and does not claim to be
			}
			var any any
			if err := json.Unmarshal([]byte(got), &any); err != nil {
				t.Errorf("selected document is not valid JSON: %v\n%q", err, got[:min(len(got), 400)])
			}
		})
	}
}

// TestSelectionFallsBackWithAMarkerThatNamesItself drift degrades to a marked
// window, never an error and never a silent one. The marker must be
// distinguishable from the client's downstream truncationMark, or an
// investigation cannot tell which layer cut.
func TestSelectionFallsBackWithAMarkerThatNamesItself(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not json", strings.Repeat("<html>not json</html>", 8000)},
		{"messages missing", `{"model":"m","prompt":"` + strings.Repeat("p", 100_000) + `"}`},
		{"messages is not an array", `{"model":"m","messages":{"role":"user"}}`},
		{"truncated json", `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("t", 100_000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := selectModelCallRequest(tc.body)
			if !strings.Contains(got, markerPrefix) {
				t.Errorf("a body the selector could not bind was stored unmarked: %q", got[:min(len(got), 200)])
			}
			if !strings.Contains(got, "select") {
				t.Errorf("the fallback marker does not say the SELECTOR is what fell back, so it cannot be "+
					"told apart from a downstream truncation: %q", got[:min(len(got), 200)])
			}
		})
	}
}

// TestTheFallbackKeepsTheTail an unbindable body is still a conversation, so the
// same reasoning that makes selection keep the newest messages makes the fallback
// keep the end.
func TestTheFallbackKeepsTheTail(t *testing.T) {
	const newest = "TAIL_OF_AN_UNBINDABLE_BODY"
	got := selectModelCallRequest(strings.Repeat("z", 200_000) + newest)

	if !strings.HasSuffix(got, newest) {
		t.Errorf("the fallback dropped the tail; got the last 80 bytes as %q", got[max(0, len(got)-80):])
	}
}

// TestASmallCompleteBodyIsStoredWholeAndUnmarked selection must not announce
// itself on a body that fits. A note claiming drops that did not happen is the
// same class of defect as a marker claiming a cut that did not happen.
func TestASmallCompleteBodyIsStoredWholeAndUnmarked(t *testing.T) {
	const body = `{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}],"max_tokens":1024}`
	got := selectModelCallRequest(body)

	if strings.Contains(got, markerPrefix) {
		t.Errorf("a complete small body was marked: %q", got)
	}
	if !strings.Contains(got, "hello") {
		t.Errorf("a complete small body lost its content: %q", got)
	}
	// The note may legitimately be present here and say `max_tokens` was dropped,
	// because it WAS. What it must never do is claim a conversation was cut when
	// the whole conversation is stored -- a note claiming a loss that did not
	// happen is the same defect as a marker claiming a cut that did not happen.
	var doc struct {
		Selection *struct {
			DroppedMessages int      `json:"dropped_messages"`
			DroppedKeys     []string `json:"dropped_keys"`
		} `json:"openbox_selection"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("selected document is not valid JSON: %v", err)
	}
	if doc.Selection != nil && doc.Selection.DroppedMessages != 0 {
		t.Errorf("dropped_messages = %d for a body whose entire history is stored: %q",
			doc.Selection.DroppedMessages, got)
	}
}

// TestNoNoteAppearsWhenNothingAtAllWasDropped the note is evidence of a
// decision, so a document that dropped nothing must not carry one.
func TestNoNoteAppearsWhenNothingAtAllWasDropped(t *testing.T) {
	const body = `{"model":"claude-opus-5","system":"be brief","messages":[{"role":"user","content":"hello"}]}`
	got := selectModelCallRequest(body)

	if strings.Contains(got, "openbox_selection") {
		t.Errorf("a document that dropped nothing carries a selection note: %q", got)
	}
}

// TestAnAllASCIIBodyAtTheCapIsNotPresentedAsComplete is the latent second mode:
// for an all-ASCII body, 65,536 runes == 65,536 bytes, so the client's byte cap
// returned its input unchanged and NO marker appeared at all -- a head window
// presented as a whole body. The selector removes the condition by never handing
// that layer a body at the cap.
func TestAnAllASCIIBodyAtTheCapIsNotPresentedAsComplete(t *testing.T) {
	const newest = "NEWEST_AT_THE_CAP"
	// Sized so the assembled body lands just over the downstream 65,536-byte net.
	body := corpusShapedRequest(t, 12, 5000, newest)
	if len(body) <= 65536 {
		t.Fatalf("fixture is %d bytes; it must exceed the 65,536 net to exercise this mode", len(body))
	}

	got := selectModelCallRequest(body)

	if len(got) >= 65536 {
		t.Errorf("selection returned %d bytes; it must leave headroom under the 65,536 net so the "+
			"downstream cap cannot silently head-cut the tail back off", len(got))
	}
	if !strings.Contains(got, newest) {
		t.Error("the newest turn was lost at the cap boundary -- the exact mode that stored no marker")
	}
	if !strings.Contains(got, "dropped_messages") {
		t.Error("a body that did not fit was stored without saying so; that is the unmarked-window mode")
	}
}

// TestASingleMessageLargerThanTheBudgetKeepsItsTail one pasted file can be the
// whole history. Dropping it would store an empty conversation for the call that
// most needs evidence.
func TestASingleMessageLargerThanTheBudgetKeepsItsTail(t *testing.T) {
	const newest = "END_OF_THE_HUGE_MESSAGE"
	body := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("q", 200_000) + newest + `"}]}`

	got := selectModelCallRequest(body)

	if !strings.Contains(got, newest) {
		t.Errorf("a single over-budget message lost its tail; kept %d bytes", len(got))
	}
	if len(got) >= 65536 {
		t.Errorf("selection returned %d bytes, over the net", len(got))
	}
}

// TestAnEscapingHeavyNewestMessageStillStoresConversation is the field defect
// selection exists to remove, and the one the suite could not see.
//
// 26.5% of measured model calls stored no conversation at all. The mechanism is
// arithmetic, not structure: tailAsJSONString budgeted a FIXED 16 bytes for what
// re-encoding would add, while json.Marshal expands `"` and `\` 2x and a literal
// `<` or a control byte 6x -- so the element it returned outran the room it had
// been measured against. The document then failed the belt check, and the
// selection became a window over the WHOLE body: with `tools` trailing and tool
// schemas larger than the budget, that window is boilerplate and nothing else.
//
// The suite missed it because every escaping case was a SINGLE-message body,
// where a window over the body does hold the newest content. It takes a
// corpus-shaped body -- many messages, `tools` trailing -- to tell the two apart.
func TestAnEscapingHeavyNewestMessageStillStoresConversation(t *testing.T) {
	const sentinel = "THE_NEWEST_TURN_XYZZY"
	// An HTML paste as the newest turn: an entirely ordinary thing to put in a
	// prompt, and every `<` of it is six bytes before this selector sees it.
	newest := strings.Repeat("<", 20_000) + sentinel
	body := corpusShapedRequestWithTools(t, 40, 2_600, newest, 700)

	got := selectModelCallRequest(body)

	if len(got) > selectionBudget {
		t.Errorf("selection returned %d bytes, over the %d budget", len(got), selectionBudget)
	}
	if !strings.Contains(got, sentinel) {
		t.Errorf("the newest turn is absent from %d stored bytes: the call that most needs "+
			"evidence stored none of it", len(got))
	}
	if strings.Contains(got, "input_schema") {
		t.Errorf("the stored value carries tool definitions, which is what a window over the " +
			"whole body keeps when `tools` trails the conversation")
	}
}

// TestTheOverBudgetTailEncodeFitsTheRoomItWasGiven pins the arithmetic on the
// element itself, because the document-level assertions can pass for the wrong
// reason: a fallback is bounded and valid JSON-or-marker too. The greedy fill
// charges an element len(element)+1, so coming in under room-1 is what keeps the
// belt check unreached and the conversation in the document.
func TestTheOverBudgetTailEncodeFitsTheRoomItWasGiven(t *testing.T) {
	for _, tc := range []struct{ name, filler string }{
		{"angle brackets", "<"},                   // 6x: \u003c
		{"ampersands", "&"},                       // 6x
		{"quotes", `"`},                           // 2x
		{"backslashes", `\`},                      // 2x
		{"control characters", "\x01"},            // 6x
		{"multibyte runes", "\u65e5\u672c\u8a9e"}, // 1x, but a tail cut lands mid-rune
		{"plain ascii", "q"},                      // 1x, the ordinary case
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, room := range []int{96, 512, 4096, selectionBudget} {
				message := json.RawMessage(strings.Repeat(tc.filler, room*8))
				got := tailAsJSONString(message, room)

				if len(got) > room-1 {
					t.Errorf("room %d: the element is %d bytes, over the %d the greedy fill "+
						"measured it against", room, len(got), room-1)
				}
				var decoded string
				if err := json.Unmarshal(got, &decoded); err != nil {
					t.Fatalf("room %d: the element is not a JSON string, so the document "+
						"cannot parse: %v", room, err)
				}
				if !strings.HasPrefix(decoded, markerPrefix) {
					t.Errorf("room %d: the element does not report that a cut happened: %q",
						room, decoded[:min(len(decoded), 80)])
				}
			}
		})
	}
}

// TestATailEncodeWithNoRoomForTheNoteKeepsTheNote records the one input this
// encode cannot satisfy, so the behaviour reads as chosen. Below about 72 bytes
// the marker alone does not fit, and the element is returned marked and oversized
// rather than silently empty -- the belt check then windows the document, which is
// where an honest bounded answer comes from at that size.
func TestATailEncodeWithNoRoomForTheNoteKeepsTheNote(t *testing.T) {
	got := tailAsJSONString(json.RawMessage(strings.Repeat("q", 4096)), 8)

	var decoded string
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("the element is not a JSON string: %v", err)
	}
	if !strings.HasPrefix(decoded, markerPrefix) {
		t.Errorf("an element with no room to explain itself was stored unmarked: %q", decoded)
	}
	if strings.Contains(decoded, "q") {
		t.Errorf("the note was padded with content that did not fit: %q", decoded)
	}
}

// literalMarkupBody assembles the body BY HAND, because every other fixture in
// this package is built with json.Marshal -- which pre-escapes `<`, `>` and `&`
// and is therefore structurally incapable of expressing the defect below. A real
// provider body carries them LITERALLY: JavaScript's JSON.stringify does not
// HTML-escape, and Go's json.Marshal does.
func literalMarkupBody(t *testing.T, count, perMessage int, newest string) string {
	t.Helper()
	unit := "<p>a & b</p>"
	filler := strings.Repeat(unit, perMessage/len(unit)+1)[:perMessage]
	msgs := make([]string, 0, count)
	for i := range count {
		text := filler
		if i == count-1 {
			text = newest
		}
		msgs = append(msgs, `{"role":"user","content":[{"type":"text","text":"`+text+`"}]}`)
	}
	tools := strings.Repeat(`{"name":"Bash","description":"run it","input_schema":{"type":"object"}},`, 700)
	return `{"model":"claude-opus-5","messages":[` + strings.Join(msgs, ",") +
		`],"system":[{"type":"text","text":"` + strings.Repeat("s", 8192) + `"}],"tools":[` +
		strings.TrimSuffix(tools, ",") + `],"stream":true}`
}

// TestLiteralMarkupInKeptMessagesDoesNotCostTheSelection is the SECOND expander,
// and the one a naive size estimate misses.
//
// keepNewestMessages charges each element len(element)+1 against `room`,
// measuring the RAW RawMessage. But json.Marshal re-escapes while it compacts a
// RawMessage it re-emits, so a literal `<`, `>` or `&` inside a kept message
// becomes a six-byte \uXXXX form in the final document: measured, 1,000 literal
// `<` cost +5,015 bytes. The document therefore exceeds a budget the fill
// believed it had met, the belt fires, and the selection is a window again --
// no matter how well the over-budget tail encode behaves.
//
// So "kept elements are re-emitted verbatim, and compaction can only shrink" is
// false, and this case exists to keep it false out loud.
func TestLiteralMarkupInKeptMessagesDoesNotCostTheSelection(t *testing.T) {
	const newest = "THE_NEWEST_TURN_WITH_MARKUP"
	body := literalMarkupBody(t, 40, 2600, newest)

	got := selectModelCallRequest(body)

	if len(got) > selectionBudget {
		t.Errorf("selection returned %d bytes, over the %d budget", len(got), selectionBudget)
	}
	if strings.HasPrefix(got, markerPrefix) {
		t.Fatalf("a body whose only peculiarity is literal markup fell back to a window: %q",
			got[:min(len(got), 160)])
	}
	if !strings.Contains(got, newest) {
		t.Errorf("the newest turn is absent from %d stored bytes", len(got))
	}
	if strings.Contains(got, "input_schema") {
		t.Error("the stored value carries tool definitions")
	}
}

// TestAMixedDensityOverBudgetTailStillKeepsConversation the escaping in a tail is
// not evenly spread, and the rescale is what has to survive that.
//
// The loop measures the expansion of the WHOLE tail, but the tail is cut from the
// END. When the expanding bytes sit at the end -- a long plain paste that finishes
// with markup -- the measured ratio under-predicts the retained suffix's own
// density, so each round improves the estimate instead of landing it. Left to
// proportional rounds alone the loop runs out and returns its note ALONE: a valid,
// unmarked, budget-fitting document carrying no conversation whatsoever, which is
// a worse answer than the marked fallback it replaced.
func TestAMixedDensityOverBudgetTailStillKeepsConversation(t *testing.T) {
	const room = 48_000
	for _, tc := range []struct{ name, tail string }{
		{"plain then markup", strings.Repeat("q", 150_000) + strings.Repeat("<", 20_000)},
		{"plain then a little markup", strings.Repeat("q", 150_000) + strings.Repeat("<", 5_000)},
		{"markup then plain", strings.Repeat("<", 20_000) + strings.Repeat("q", 150_000)},
		{"uniform markup", strings.Repeat("<", 200_000)},
		{"uniform plain", strings.Repeat("q", 200_000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tailAsJSONString(json.RawMessage(tc.tail), room)

			if len(got) > room-1 {
				t.Errorf("the element is %d bytes, over the %d it was measured against",
					len(got), room-1)
			}
			var decoded string
			if err := json.Unmarshal(got, &decoded); err != nil {
				t.Fatalf("the element is not a JSON string: %v", err)
			}
			if kept := len(decoded); kept < 1000 {
				t.Errorf("the element kept %d bytes of the message beyond its own note; a "+
					"document that fits the budget while carrying no conversation is the "+
					"unmarked-total-loss case, not a success", kept)
			}
		})
	}
}

// TestAMarkedBodyThatRedactionGrewIsReWindowed pins the answer to the one
// question this area left open: what bounds a FALLBACK after redaction has grown
// it.
//
// Nothing did. Pass 1 falls back within the budget, redaction then grows the
// bytes because a placeholder is longer than the short secret it replaces, and
// the recovery selection hands a marked body straight back -- so 4 measured rows
// reached the store over the budget, with only capRunes left to bound them. And
// capRunes head-cuts, so the failure mode was losing the END of a tail window
// while its head marker still said the tail was kept.
func TestAMarkedBodyThatRedactionGrewIsReWindowed(t *testing.T) {
	const newest = "THE END OF THE MARKED WINDOW"
	// Pass 1's own output for an unbindable body: a marked tail window inside the
	// budget, dense in SHORT secrets, which is what makes redaction grow it.
	marked := selectModelCallRequest("not json " + strings.Repeat("pwd:aaaaaaaa ", 4000) + newest)
	if !strings.HasPrefix(marked, markerPrefix) {
		t.Fatalf("the fixture is not a marked body: %q", marked[:min(len(marked), 120)])
	}
	if len(marked) > selectionBudget {
		t.Fatalf("pass 1 returned %d bytes, already over the %d budget", len(marked), selectionBudget)
	}

	got := captureRequestBody(marked)

	if len(got) > selectionBudget {
		t.Errorf("stored %d bytes, over the %d budget: the selector returns a marked body "+
			"untouched, so nothing re-trimmed what redaction grew", len(got), selectionBudget)
	}
	if !strings.HasPrefix(got, markerPrefix) {
		t.Errorf("the re-cut took the marker off, leaving an unmarked window: %q",
			got[:min(len(got), 120)])
	}
	if !strings.HasSuffix(got, newest) {
		t.Errorf("the re-cut kept the head, so the marker's claim that the tail was kept is "+
			"now false; last 80 bytes: %q", got[max(0, len(got)-80):])
	}
}

// TestAReasonStringCannotMoveTheMarkerBoundary the marked-body convention is a
// prefix ending at the first `]`, and rewindowMarkedBody recovers the boundary by
// finding it. Today's four reason strings contain no `]`, which makes the parse
// correct by luck rather than by construction -- and Go's own JSON syntax errors
// quote the offending character in brackets, so folding one into a reason is an
// obvious improvement that would silently corrupt the marker.
func TestAReasonStringCannotMoveTheMarkerBoundary(t *testing.T) {
	const newest = "THE TAIL OF THE BODY"
	marked := fallbackWindow(strings.Repeat("z", 200_000)+newest,
		`invalid character ']' looking for beginning of value`)

	if !strings.HasPrefix(marked, markerPrefix) {
		t.Fatalf("not marked: %q", marked[:min(len(marked), 120)])
	}
	end := strings.IndexByte(marked, ']')
	if end < 0 {
		t.Fatal("the marker has no close bracket at all")
	}
	if !strings.HasSuffix(marked[:end+1], "]") || strings.Contains(marked[:end+1], "invalid character ')' looking") == false {
		t.Errorf("the first `]` is not the marker's own terminator; marker read as %q", marked[:end+1])
	}
	// The round trip that actually matters: re-windowing must keep the marker and
	// the tail rather than cut inside the reason text.
	got := rewindowMarkedBody(marked + strings.Repeat("q", selectionBudget))
	if !strings.HasPrefix(got, markerPrefix) {
		t.Errorf("the re-window lost the marker: %q", got[:min(len(got), 120)])
	}
	if len(got) > selectionBudget {
		t.Errorf("the re-window returned %d bytes, over the %d budget", len(got), selectionBudget)
	}
}

// BenchmarkSelectAP50RequestBody the relay is the one component that must never
// slow the tool down. p50 request is 520,452 bytes; the requirement is to
// MEASURE this rather than argue it, against a model call that takes seconds.
func BenchmarkSelectAP50RequestBody(b *testing.B) {
	msgs := make([]string, 0, 200)
	for i := range 200 {
		msgs = append(msgs, fmt.Sprintf(`{"role":"user","content":[{"type":"text","text":%q}]}`,
			strings.Repeat("h", 2600)+fmt.Sprint(i)))
	}
	body := `{"model":"claude-opus-5","messages":[` + strings.Join(msgs, ",") +
		`],"system":[{"type":"text","text":"` + strings.Repeat("s", 8192) + `"}],"stream":true}`
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		_ = selectModelCallRequest(body)
	}
}

// TestTheNewestTurnSurvivesTheWholeCapturePath is the WIRED assertion, and it is
// the one that fails on the pre-selection tree. The unit tests above prove
// selection picks the right bytes; this proves the three caps downstream of it do
// not take them back off.
//
// The defect was never in one cap. It was the composition: capturableBody
// head-cut 256 KiB, capRunes head-cut 65,536 runes, and capModelCallRequest
// tail-cut 65,536 bytes, so the net was the tail of the head -- exactly 65,536
// bytes on all 27 measured rows. The last assertion here is the bridge to the
// client's cap: a captured body strictly under 65,536 BYTES cannot be cut by
// either remaining layer, so proving that bound locally proves the composition.
func TestTheNewestTurnSurvivesTheWholeCapturePath(t *testing.T) {
	const newest = "THE_NEWEST_TURN_WIRED"
	body := corpusShapedRequest(t, 200, 2600, newest)

	var got recorded
	upstream := upstreamRecorder(t, &got, nil)
	em := &recordingEmitter{}
	srv := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()

	// The relay's first duty: the provider still gets the whole body, byte for byte.
	if string(got.body) != body {
		t.Errorf("the relay altered the forwarded request: forwarded %d bytes, want %d", len(got.body), len(body))
	}

	captured := em.await(t, 1)[0].RequestBody
	if !strings.Contains(captured, newest) {
		t.Fatalf("the newest turn did not survive the capture path (%d bytes captured of %d sent)",
			len(captured), len(body))
	}
	if at := strings.Index(captured, newest); at < len(captured)*3/5 {
		t.Errorf("the newest turn is at byte %d of %d, inside the middle the alignment judge elides",
			at, len(captured))
	}
	if len(captured) >= 65536 {
		t.Errorf("captured %d bytes; the client's capModelCallRequest tail-cuts at 65,536 and capRunes "+
			"head-cuts before it, so anything at or over that bound is back on the boundary "+
			"selection exists to leave", len(captured))
	}
	if strings.Contains(captured, "input_schema") {
		t.Error("tool definitions reached the stored body; they are the constant that made 28 calls store 4 distinct values")
	}
}

// TestRedactionGrowthDoesNotCostTheNewestTurn pins the headroom that
// selectionBudget claims, rather than leaving it as arithmetic in a comment.
//
// The fixture is the whole test, and the first version of it was a placebo worth
// recording. It used a 43-character AWS key, which the redactor replaces with a
// 37-character placeholder: net MINUS six bytes per message. It asserted that a
// placeholder appeared, which it did, and concluded the headroom was tested. The
// body had shrunk. Measured, not reasoned about:
//
//	43-char AWS value   in=287 out=281  -6 bytes  x0.98
//	`pwd:` + 8 chars    in= 13 out= 42  +29 bytes x3.23
//	`token:` + 12 chars in= 19 out= 44  +25 bytes x2.32
//
// Growth comes from SHORT secrets, because the placeholder is a fixed ~37 bytes
// whatever it replaces. So the adversarial body is one dense in short
// keyword-adjacent values -- a pasted .env, a k8s secrets manifest -- and not a
// large one. Below the fix, this fixture selected 49,115 bytes which redaction
// grew to 83,770, and the stored result was 65,536 bytes of invalid JSON cut
// mid-token with NO marker: the unmarked-head-window mode selection exists to
// kill, resurrected inside the fix for it.
func TestRedactionGrowthDoesNotCostTheNewestTurn(t *testing.T) {
	const newest = "NEWEST_AFTER_REDACTION_GROWTH"
	// Short secrets, so redaction actually grows the document. 1,400 of them puts
	// growth well past the headroom rather than just inside it.
	msgs := make([]string, 0, 1400)
	for i := range 1400 {
		text := fmt.Sprintf("pwd:aaaaaaa%d", i%10)
		if i == 1399 {
			text = newest
		}
		one, err := json.Marshal(map[string]any{"role": "user", "content": text})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		msgs = append(msgs, string(one))
	}
	body := `{"model":"claude-opus-5","messages":[` + strings.Join(msgs, ",") + `],"stream":true}`

	var got recorded
	upstream := upstreamRecorder(t, &got, nil)
	em := &recordingEmitter{}
	srv := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()

	captured := em.await(t, 1)[0].RequestBody

	// The redactor must actually have fired AND grown the body, or this fixture
	// has regressed to the placebo it replaced.
	if !strings.Contains(captured, "OPENBOX_REDACTED") {
		t.Fatal("no redaction placeholder in the captured body; this fixture no longer exercises growth")
	}
	if len(captured) >= 65536 {
		t.Errorf("captured %d bytes, at or over the 65,536 net: redaction growth outran the recovery "+
			"and capRunes will have head-cut the tail away", len(captured))
	}
	if !json.Valid([]byte(captured)) {
		t.Errorf("captured body is not valid JSON -- a head cut through a token, which is what an "+
			"unrecovered overrun looks like: %q", captured[max(0, len(captured)-60):])
	}
	if !strings.Contains(captured, newest) {
		t.Errorf("redaction growth cost the newest turn; captured %d bytes", len(captured))
	}
	// And the loss must be REPORTED, not merely survived.
	if !strings.Contains(captured, "dropped_messages") {
		t.Error("messages were dropped to absorb redaction growth and nothing says so")
	}
}

// TestTheSelectionBudgetStaysBelowTheClientsByteNet is the guard that makes the
// downstream caps unreachable BY CONSTRUCTION rather than by arithmetic in a
// comment.
//
// It asserts against client.MaxModelCallBodyBytes, the constant the client
// actually cuts on, not a copy of its value. A duplicated literal would red only
// if someone edited the copy as well -- which is to say it would pass whether or
// not the invariant held. Import direction allows the real thing: gateway already
// imports client, and depguard names it.
//
// If these ever meet, capRunes head-cuts a selected document and the newest turn
// is dropped again. captureRequestBody's post-redaction re-select is what handles
// growth; this is what handles the budget itself.
func TestTheSelectionBudgetStaysBelowTheClientsByteNet(t *testing.T) {
	if selectionBudget >= client.MaxModelCallBodyBytes {
		t.Fatalf("selectionBudget is %d and the client cuts at %d; a selected document at or over "+
			"the net gets head-cut by capRunes, which is the defect selection removed",
			selectionBudget, client.MaxModelCallBodyBytes)
	}
	// And the gap must be big enough to be a real margin rather than a rounding
	// accident, since it is the single-pass headroom for redaction growth.
	if gap := client.MaxModelCallBodyBytes - selectionBudget; gap < 8*1024 {
		t.Errorf("only %d bytes of headroom under the net; that is thin enough that ordinary "+
			"redaction growth would push every large body onto the recovery path", gap)
	}
}

// TestAnEmptyHistoryStillReportsADroppedKey the early return for an empty
// `messages` array once discarded the note entirely, so
// `{"messages":[],"tools":[...]}` dropped the tool definitions and stored a
// document claiming nothing had gone. An empty conversation is a complete
// document; a dropped key is still a loss, and the two are independent.
func TestAnEmptyHistoryStillReportsADroppedKey(t *testing.T) {
	got := selectModelCallRequest(`{"model":"m","messages":[],"tools":[{"name":"Bash"}],"max_tokens":10}`)

	var doc struct {
		Selection *struct {
			DroppedMessages int      `json:"dropped_messages"`
			DroppedKeys     []string `json:"dropped_keys"`
		} `json:"openbox_selection"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("selected document is not valid JSON: %v", err)
	}
	if doc.Selection == nil {
		t.Fatalf("no selection note on a document that dropped tools and max_tokens: %q", got)
	}
	if len(doc.Selection.DroppedKeys) != 2 {
		t.Errorf("dropped_keys = %v, want both dropped keys named", doc.Selection.DroppedKeys)
	}
	if doc.Selection.DroppedMessages != 0 {
		t.Errorf("dropped_messages = %d; no messages existed to drop", doc.Selection.DroppedMessages)
	}
}

// TestTheRecoveryPassCarriesTheFirstPassAccountingForward the second selection --
// the one that runs when redaction grew the first result past the budget -- sees
// only the redacted copy of the first pass's OUTPUT. If it reset the note, the
// stored record would report that intermediate as `original_bytes` and file the
// first note under `dropped_keys`, understating the loss it exists to report.
func TestTheRecoveryPassCarriesTheFirstPassAccountingForward(t *testing.T) {
	// A first-pass document: 200 dropped messages out of a 541,631-byte original.
	first := `{"model":"m","openbox_selection":{"dropped_messages":200,"dropped_keys":["tools"],"original_bytes":541631},` +
		`"messages":[` + strings.Repeat(`{"role":"user","content":"`+strings.Repeat("z", 400)+`"},`, 200) +
		`{"role":"user","content":"NEWEST"}]}`

	got := selectModelCallRequest(first)

	var doc struct {
		Selection *struct {
			DroppedMessages int      `json:"dropped_messages"`
			DroppedKeys     []string `json:"dropped_keys"`
			OriginalBytes   int      `json:"original_bytes"`
		} `json:"openbox_selection"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if doc.Selection == nil {
		t.Fatal("the recovery pass dropped the note entirely")
	}
	if doc.Selection.OriginalBytes != 541631 {
		t.Errorf("original_bytes = %d, want the FIRST pass's 541631; this pass only saw the "+
			"intermediate", doc.Selection.OriginalBytes)
	}
	if doc.Selection.DroppedMessages <= 200 {
		t.Errorf("dropped_messages = %d, want more than the first pass's 200: this pass dropped "+
			"more on top", doc.Selection.DroppedMessages)
	}
	for _, k := range doc.Selection.DroppedKeys {
		if k == "openbox_selection" {
			t.Error("the first pass's note was filed as a dropped key rather than carried forward")
		}
	}
	if !strings.Contains(got, "tools") {
		t.Error("the first pass's dropped_keys were lost in the merge")
	}
	if !strings.Contains(got, "NEWEST") {
		t.Error("the recovery pass lost the newest turn")
	}
}
