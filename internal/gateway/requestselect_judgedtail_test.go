package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The alignment judge reads roughly 390-444 bytes of the stored document and
// keeps head 3/5 + tail 2/5, so the LAST element of `messages` is what it reads
// as the current goal. Measured on the live corpus, that element was not a
// conversation turn on 67% of calls -- most often a 51-132 byte `role:"system"`
// token counter the agent runtime appends -- so the judged tail carried no turn
// on two calls in three. Every case here is about which turn is worth being
// newest; the selector was already faithful about keeping it.

// judgedTailBody assembles a request around an explicit message list, with
// `tools` trailing so a note is always produced. Small by construction: these
// cases are about SHAPE, and a budget drop would confound the counter under test.
func judgedTailBody(msgs ...string) string {
	return `{"model":"claude-opus-5","messages":[` + strings.Join(msgs, ",") +
		`],"tools":[{"name":"Bash","input_schema":{"type":"object"}}]}`
}

func judgedTurn(role, text string) string {
	return fmt.Sprintf(`{"role":%q,"content":[{"type":"text","text":%q}]}`, role, text)
}

// storedDoc reads back what selection stored, so a case can assert on the
// element the judge would actually read rather than on a substring of the whole.
func storedDoc(t *testing.T, stored string) ([]json.RawMessage, *selectionNote) {
	t.Helper()
	if strings.HasPrefix(stored, markerPrefix) {
		t.Fatalf("selection fell back rather than selecting: %q", stored[:min(len(stored), 160)])
	}
	var doc struct {
		Messages  []json.RawMessage `json:"messages"`
		Selection *selectionNote    `json:"openbox_selection"`
	}
	if err := json.Unmarshal([]byte(stored), &doc); err != nil {
		t.Fatalf("the stored document is not valid JSON: %v", err)
	}
	if len(doc.Messages) == 0 {
		t.Fatal("the stored document has an empty history, which is the one outcome " +
			"selection must never produce silently")
	}
	return doc.Messages, doc.Selection
}

// TestATrailingTokenCounterDoesNotBecomeTheJudgedTurn is the case the phase
// exists for: the newest element the provider sent is a token counter, and the
// judge would read it as the current goal.
func TestATrailingTokenCounterDoesNotBecomeTheJudgedTurn(t *testing.T) {
	const newest = "THE_REAL_NEWEST_TURN"
	stored := selectModelCallRequest(judgedTailBody(
		judgedTurn("user", "an older question"),
		judgedTurn("assistant", "an older answer"),
		judgedTurn("user", newest),
		`{"role":"system","content":"<total_tokens>15000 tokens left</total_tokens>"}`,
	))

	messages, note := storedDoc(t, stored)
	last := messages[len(messages)-1]

	// Asserted with the SHIPPED predicate, not a re-implementation of it: a
	// proxy that agrees with the rule on this fixture can still disagree in
	// the field, which is how the earlier content-based reading scored 140
	// destroyed turns as wins.
	if !isTurn(last) {
		t.Errorf("the element the judge reads as the current goal is not a turn: %s", last)
	}
	if !strings.Contains(string(last), newest) {
		t.Errorf("the newest real turn is not last; the judged element is %s", last)
	}
	if note == nil || note.SkippedTrailingNonTurns != 1 {
		t.Errorf("the skip was not reported: note = %+v", note)
	}
	if note != nil && note.DroppedMessages != 0 {
		t.Errorf("a non-turn skip was filed as a budget drop (dropped_messages = %d); a reader "+
			"cannot then tell a capacity problem from a shape problem", note.DroppedMessages)
	}
	// The judge keeps head 3/5 + tail 2/5, so presence is not enough.
	if at := strings.Index(stored, newest); at < len(stored)*3/5 {
		t.Errorf("the newest turn is at byte %d of %d -- inside the middle the judge elides", at, len(stored))
	}
}

// TestAnAllSyntheticHistoryKeepsItsNewestElement pins the never-empty rule.
// Measured at 1% of corpus documents: there is no real turn to promote, and
// storing an empty conversation would be worse than storing a synthetic element.
func TestAnAllSyntheticHistoryKeepsItsNewestElement(t *testing.T) {
	const only = "THE_ONLY_ELEMENT_THERE_IS"
	stored := selectModelCallRequest(judgedTailBody(
		`{"role":"system","content":"an earlier reminder"}`,
		fmt.Sprintf(`{"role":"system","content":%q}`, only),
	))

	messages, note := storedDoc(t, stored)

	if !strings.Contains(string(messages[len(messages)-1]), only) {
		t.Errorf("an all-synthetic history lost its newest element; last is %s",
			messages[len(messages)-1])
	}
	if note != nil && note.SkippedTrailingNonTurns != 0 {
		t.Errorf("nothing could be skipped, but %d was reported: a note claiming a loss that "+
			"did not happen is the same defect as an unreported one",
			note.SkippedTrailingNonTurns)
	}
}

// TestAToolResultNewestIsLeftAlone Anthropic carries a tool result inside a
// `role:"user"` message, so the role rule keeps every one of them -- and
// tool_result is the most common newest element in the corpus replay. A content
// rule is what would have put these at risk.
func TestAToolResultNewestIsLeftAlone(t *testing.T) {
	const output = "THE_TOOL_OUTPUT"
	stored := selectModelCallRequest(judgedTailBody(
		judgedTurn("assistant", "I will run it"),
		fmt.Sprintf(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":%q}]}`, output),
	))

	messages, note := storedDoc(t, stored)

	if !strings.Contains(string(messages[len(messages)-1]), output) {
		t.Errorf("a tool result was dropped from the judged tail; last is %s",
			messages[len(messages)-1])
	}
	if note != nil && note.SkippedTrailingNonTurns != 0 {
		t.Errorf("a tool result was counted as a non-turn (%d skipped)",
			note.SkippedTrailingNonTurns)
	}
}

// TestTheWalkBackStopsAtItsCap the walk-back is bounded, so a history that
// trails many non-turns is truncated by a constant rather than scanned to the
// start -- and the reported count is what makes the cap visible when it binds.
func TestTheWalkBackStopsAtItsCap(t *testing.T) {
	msgs := []string{judgedTurn("user", "the real turn")}
	for i := range 10 {
		msgs = append(msgs, fmt.Sprintf(`{"role":"system","content":"reminder %d"}`, i))
	}
	stored := selectModelCallRequest(judgedTailBody(msgs...))

	messages, note := storedDoc(t, stored)

	if note == nil || note.SkippedTrailingNonTurns != maxTrailingNonTurnSkip {
		t.Fatalf("the walk-back did not stop at its cap of %d: note = %+v",
			maxTrailingNonTurnSkip, note)
	}
	if got := len(messages); got != len(msgs)-maxTrailingNonTurnSkip {
		t.Errorf("kept %d elements, want %d: the cap and the history must agree",
			got, len(msgs)-maxTrailingNonTurnSkip)
	}
	// The cap bound before a turn was reached, so the newest element is still a
	// non-turn. That is the honest outcome: bounded work, reported loss.
	if isTurn(messages[len(messages)-1]) {
		t.Error("the fixture no longer exercises the cap; more than the cap's worth of " +
			"non-turns must trail for this case to mean anything")
	}
}

// TestAnUnclassifiableElementIsTreatedAsATurn selection may drop what it cannot
// fit. It must never drop what it cannot classify.
func TestAnUnclassifiableElementIsTreatedAsATurn(t *testing.T) {
	const newest = "AN_ELEMENT_THAT_WILL_NOT_BIND"
	stored := selectModelCallRequest(judgedTailBody(
		judgedTurn("user", "an older question"),
		fmt.Sprintf(`[%q]`, newest), // an array, not an object: role cannot be read
	))

	messages, note := storedDoc(t, stored)

	if !strings.Contains(string(messages[len(messages)-1]), newest) {
		t.Errorf("an element whose role could not be read was dropped; last is %s",
			messages[len(messages)-1])
	}
	if note != nil && note.SkippedTrailingNonTurns != 0 {
		t.Errorf("an unbindable element was classified as a non-turn (%d skipped)",
			note.SkippedTrailingNonTurns)
	}
}

// TestTheSkipCountAccumulatesAcrossPasses the recovery pass that runs when
// redaction grows a document past the budget must ADD to the first pass's count.
// Overwriting it would report the second pass's skip as the total and understate
// the loss, which is the defect priorNote exists to prevent for the other keys.
func TestTheSkipCountAccumulatesAcrossPasses(t *testing.T) {
	first := `{"model":"m","openbox_selection":{"dropped_messages":0,"skipped_trailing_non_turns":1,` +
		`"original_bytes":600000},"messages":[{"role":"user","content":"REAL_TURN"},` +
		`{"role":"system","content":"a reminder the first pass did not see"}]}`

	_, note := storedDoc(t, selectModelCallRequest(first))

	if note == nil {
		t.Fatal("the recovery pass dropped the note entirely")
	}
	if note.SkippedTrailingNonTurns != 2 {
		t.Errorf("skipped_trailing_non_turns = %d, want 2: the first pass's 1 plus this pass's 1",
			note.SkippedTrailingNonTurns)
	}
	if note.OriginalBytes != 600000 {
		t.Errorf("original_bytes = %d, want the first pass's 600000", note.OriginalBytes)
	}
}
