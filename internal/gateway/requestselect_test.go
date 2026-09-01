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
	tools := strings.Repeat(`{"name":"Bash","description":"run a shell command","input_schema":{"type":"object"}},`, 60)
	tools = "[" + strings.TrimSuffix(tools, ",") + "]"
	return fmt.Sprintf(`{"model":"claude-opus-5","messages":%s,"system":[{"type":"text","text":%q}],"tools":%s,"max_tokens":32000,"stream":true}`,
		msgsJSON, strings.Repeat("s", 8192), tools)
}

// TestTheNewestTurnSurvivesSelection is the phase's load-bearing assertion and
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

// TestMessagesIsTheLastKeyInTheSelectedDocument is the trap this phase exists to
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
			"failure mode this phase is repairing, not a smaller version of it")
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

// BenchmarkSelectAP50RequestBody the relay is the one component that must never
// slow the tool down. p50 request is 520,452 bytes; the plan's requirement was to
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
			"head-cuts before it, so anything at or over that bound is back on the boundary this "+
			"phase exists to leave", len(captured))
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
// mid-token with NO marker: the unmarked-head-window mode this phase exists to
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
			"the net gets head-cut by capRunes, which is the defect this phase removed",
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
