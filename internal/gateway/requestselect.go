package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// markerPrefix is what every honest "this is not the body" string in the capture
// path starts with, and the selector's short-circuit for one.
const markerPrefix = "[openbox: "

// selectionBudget is deliberately BELOW the 65,536-byte net the client's
// capModelCallRequest enforces, and the gap is the point rather than slack.
//
// The chain that produced the defect was three caps composing head/head/tail:
// capturableBody head-cut 256 KiB, capRunes head-cut 65,536 RUNES, and only then
// did capModelCallRequest tail-cut 65,536 BYTES -- so the net was the tail of the
// head, exactly 65,536 bytes on every one of 27 measured rows, dropping only the
// first few hundred. Selecting to the same 65,536 would put the result right back
// on that boundary: redaction runs AFTER selection and can grow a body (a
// placeholder is longer than the shortest value it replaces), and one byte of
// growth hands capRunes a head cut that takes the newest turn -- the one thing
// selection exists to keep -- straight back off.
//
// 48 KiB leaves 16 KiB of headroom under that net, about 33%, which covers the
// ordinary case in one pass.
//
// It is NOT a sufficient bound, and an earlier version of this comment claimed it
// was. The placeholder is a fixed ~37 bytes whatever it replaces, so growth scales
// with the NUMBER of secrets, not their length: `pwd:` plus eight characters goes
// from 13 bytes to 42, a factor of 3.23. Enough short secrets outrun any fixed
// headroom, and 1,400 of them did -- 49,115 bytes selected, 83,770 after
// redaction. So the guarantee comes from captureRequestBody re-selecting once on
// the redacted text rather than from this number, and
// TestRedactionGrowthDoesNotCostTheNewestTurn exercises real growth rather than
// asserting that a placeholder appeared.
const selectionBudget = 48 * 1024

// modelBudget bounds the `model` field, which was copied verbatim on the grounds
// that a model id is short. It is short in every honest request -- and this
// selector's whole subject is what happens when a request is not honest.
//
// Uncapped, one oversized `model` value drives `room` in keepNewestMessages
// negative, so no message fits, the belt check fires, and the window is over a
// document the model field dominates. Measured on a 60 KB `model` with a
// 16-byte conversation: the turn was absent from all 49,152 stored bytes. That is
// the "stores boilerplate instead of conversation" defect this file exists to
// remove, reached through the one top-level field that had no bound.
//
// 256 bytes is far above any real model id and far below anything that could
// crowd out a turn.
const modelBudget = 256

// systemBudget caps the system prompt to a head window. It is the most valuable
// non-conversation field -- it is what the agent was told it is -- but it is also
// near-constant between calls, so it earns a few KB and not more.
const systemBudget = 4 * 1024

// selectedRequest is a STRUCT and not a map, and the field order below is the
// whole reason.
//
// json.Marshal sorts a MAP's keys, which would emit "messages" first, ahead of
// "model", "openbox_selection" and "system". The alignment judge sees roughly
// 390-444 bytes of this document and keeps them as head 3/5 + tail 2/5
// (elideMiddle, openbox-core goal_alignment_session.go:773-786), so a sorted
// document puts the newest turn in precisely the middle the judge discards.
// Declaring Messages last is what lands it in the tail the judge keeps -- and it
// is why this cannot be built with a map, however much more convenient that
// would be.
type selectedRequest struct {
	Model     json.RawMessage   `json:"model,omitempty"`
	System    json.RawMessage   `json:"system,omitempty"`
	Selection *selectionNote    `json:"openbox_selection,omitempty"`
	Messages  []json.RawMessage `json:"messages"`
}

// selectionNote records what selection removed. Loss is acceptable; unreported
// loss is not, which is the same rule the spool's .discarded record follows.
type selectionNote struct {
	DroppedMessages int `json:"dropped_messages"`
	// SkippedTrailingNonTurns is its OWN key and not part of DroppedMessages.
	// A budget drop and a non-turn skip are different losses -- one means the
	// conversation outgrew the budget, the other means the newest element was
	// not a turn -- and a reader who cannot tell them apart cannot tell a
	// capacity problem from a shape problem.
	SkippedTrailingNonTurns int      `json:"skipped_trailing_non_turns,omitempty"`
	DroppedKeys             []string `json:"dropped_keys,omitempty"`
	OriginalBytes           int      `json:"original_bytes"`
}

// selectModelCallRequest picks the bytes worth storing out of a model-call
// request body, instead of cutting a window into one.
//
// The measurement that motivates it: `messages` is p50 82.1% of a request body,
// 95.3% of bodies exceed 64 KiB, and key order varies (65.5% put `messages`
// second with `tools` trailing, 27.1% put it after `system`). So NO byte window
// from either end reliably contains the newest turn, which is the only part that
// changes between calls -- 28 measured calls stored 4 distinct bodies, and
// `"messages"` appeared in 0 of 27.
//
// The bind is tolerant by design, never a schema: one key name on a top-level
// object. Provider drift, a non-JSON body, a truncated one, or anything that is
// not a conversation degrades to a marked tail window. It never errors, because
// this runs in the request path of the one component that must not break the
// tool.
func selectModelCallRequest(body string) string {
	if body == "" {
		return ""
	}
	// A marker from the decode step is already the honest answer; re-marking it
	// would just stack two claims about the same body.
	if len(body) >= len(markerPrefix) && body[:len(markerPrefix)] == markerPrefix {
		return body
	}

	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		return fallbackWindow(body, "what reached the selector did not parse as a JSON object")
	}
	rawMessages, ok := fields["messages"]
	if !ok {
		return fallbackWindow(body, "no messages array in what reached the selector")
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(rawMessages, &messages); err != nil {
		return fallbackWindow(body, "messages is not an array")
	}

	doc := selectedRequest{
		Model:  cappedField(fields["model"], modelBudget, "model id"),
		System: cappedField(fields["system"], systemBudget, "system prompt"),
	}
	dropped := droppedKeys(fields)
	// Before the fill and not after: dropping a trailing non-turn here returns its
	// bytes to `room`, so one more real turn can fit.
	messages, skipped := trimTrailingNonTurns(messages)
	// A SECOND pass over an already-selected document -- the recovery pass that runs
	// when redaction grew the first result past the budget -- has to carry the first
	// pass's accounting forward instead of resetting it.
	prior := priorNote(fields["openbox_selection"])

	// Overhead first, so the message budget is what is actually left rather than
	// an estimate. Marshalling the document with an empty history costs one small
	// allocation and removes the guesswork.
	kept, note, ok := keepNewestMessages(&doc, messages, len(body), dropped, prior, skipped)
	if !ok {
		// The budget could not be established, so any document built here would be
		// bounded by nothing. Falling back is the only answer that stays honest:
		// returning the history unmeasured would be an unmarked loss, which is the
		// exact class of defect this selector exists to remove.
		return fallbackWindow(body, "the selection budget could not be established")
	}
	doc.Messages = kept
	doc.Selection = note

	out, err := marshalNoHTMLEscape(doc)
	if err != nil {
		// Unreachable in practice: every field is either a validated RawMessage or
		// a plain scalar. Falling back rather than panicking keeps the relay's one
		// hard rule -- never break the tool -- ahead of this function's ambitions.
		return fallbackWindow(body, "the selected document could not be encoded")
	}
	if len(out) > selectionBudget {
		// Belt: keepNewestMessages fits the budget by construction, so reaching here
		// means an element's re-encode outran the room it was measured against, or
		// the overhead alone exceeds the budget.
		//
		// The window is cut from the DOCUMENT and not from `body`, and that is the
		// whole difference between storing conversation and storing nothing. The
		// document has already dropped `tools` and capped `system`, so its tail is
		// the newest turn; a window over the raw body keeps whatever key trails it,
		// which is `tools` in 65.5% of corpus bodies -- the one thing selection
		// exists to discard. Measured: this path stored zero conversation on 26.5%
		// of model calls.
		return fallbackWindow(string(out), documentSourcedFallback)
	}
	return string(out)
}

// keepNewestMessages fills the budget greedily FROM THE END. Newest-first is the
// entire ordering argument of this phase: the last entry is the turn that
// distinguishes this call from the previous one.
//
// The bool is whether the budget could be established at all. It is not
// decoration: without it a marshal failure here returned a nil slice, which
// json.Marshal renders as `"messages":null` -- a silently emptied conversation
// carrying no marker, in the one function whose job is to make loss visible.
func keepNewestMessages(doc *selectedRequest, messages []json.RawMessage, originalBytes int, dropped []string, prior *selectionNote, skipped int) ([]json.RawMessage, *selectionNote, bool) {
	// An empty history is a complete document -- but a dropped KEY is a loss too,
	// and so is a prior pass's. Reporting only the message count would let
	// `{"messages":[],"tools":[...]}` drop `tools` while claiming nothing went.
	if len(messages) == 0 {
		if len(dropped) == 0 && prior == nil && skipped == 0 {
			return []json.RawMessage{}, nil, true
		}
		return []json.RawMessage{}, mergeNote(prior, selectionNote{
			DroppedKeys:             dropped,
			OriginalBytes:           originalBytes,
			SkippedTrailingNonTurns: skipped,
		}), true
	}

	probe := *doc
	probe.Messages = []json.RawMessage{}
	probe.Selection = mergeNote(prior, selectionNote{
		DroppedMessages:         len(messages),
		DroppedKeys:             dropped,
		OriginalBytes:           originalBytes,
		SkippedTrailingNonTurns: skipped,
	})
	skeleton, err := marshalNoHTMLEscape(probe)
	if err != nil {
		return nil, nil, false
	}
	room := selectionBudget - len(skeleton)

	keep := make([]json.RawMessage, 0, len(messages))
	used := 0
	for i := len(messages) - 1; i >= 0; i-- {
		cost := len(messages[i]) + 1 // the comma or the bracket it replaces
		if used+cost > room {
			break
		}
		used += cost
		keep = append(keep, messages[i])
	}
	// Greedy from the end built it backwards; the stored document must read in
	// conversation order or a reader cannot tell which turn is newest.
	for l, r := 0, len(keep)-1; l < r; l, r = l+1, r-1 {
		keep[l], keep[r] = keep[r], keep[l]
	}

	truncated := false
	if len(keep) == 0 {
		// One message larger than the whole budget: a pasted file can be the entire
		// history. Storing an empty conversation for the call that most needs
		// evidence is the worse answer, so keep that message's TAIL as a string.
		keep = append(keep, tailAsJSONString(messages[len(messages)-1], room))
		// That rewrote the element: its role and content object are gone, replaced
		// by a marked string. The element says so itself, but the DOCUMENT must say
		// so too -- otherwise a single over-budget message counts as
		// len(keep) == len(messages), takes the "nothing was dropped" exit below,
		// and the note is absent from a document that lost most of a message.
		truncated = true
	}

	if len(keep) == len(messages) && len(dropped) == 0 && prior == nil && skipped == 0 && !truncated {
		// Nothing was dropped, so a note would claim a loss that did not happen --
		// the same class of defect as a marker claiming a cut that did not happen.
		return keep, nil, true
	}
	return keep, mergeNote(prior, selectionNote{
		DroppedMessages:         len(messages) - len(keep),
		DroppedKeys:             dropped,
		OriginalBytes:           originalBytes,
		SkippedTrailingNonTurns: skipped,
	}), true
}

// marshalNoHTMLEscape encodes without json.Marshal's HTML escaping, and the
// reason is arithmetic rather than taste.
//
// json.Marshal rewrites `<`, `>` and `&` into their six-byte \uXXXX forms during
// the compaction pass -- INCLUDING inside a json.RawMessage it is merely
// re-emitting. keepNewestMessages charges each element len(element)+1 against the
// room it measured, so every literal `<` in a kept message made the final
// document five bytes bigger than the fill believed it had built. The belt then
// fired on a budget that had in fact been met, and the selection degraded to a
// window over the raw body -- which is `tools`. Measured: 1,000 literal `<` cost
// +5,015 bytes.
//
// Provider bodies carry those bytes literally: JavaScript's JSON.stringify does
// not HTML-escape and Go's encoder does, which is also why every fixture in this
// package built with json.Marshal was structurally unable to reproduce it.
//
// Turning the escaping off makes the measured size and the encoded size the same
// number, which is the property the entire budget rests on. Nothing downstream
// cares: HTML escaping is a transport encoding of identical characters, and this
// document travels onward as a JSON string value that its own encoder escapes
// again.
func marshalNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	// Encode appends a newline that Marshal does not.
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}

// maxTailEncodeRounds bounds the proportional rescale. Small on purpose: each
// round only refines an estimate, and the terminal cut that follows fits
// unconditionally, so further rounds would buy a few more bytes on a branch that
// is already pathological.
const maxTailEncodeRounds = 3

// maxEscapeExpansion is the most bytes ONE input byte can encode to: a byte below
// 0x20 becomes a six-byte \u00XX form. `"` and `\` cost 2 and an invalid UTF-8
// byte costs 3, so six is the ceiling for any input whatsoever.
const maxEscapeExpansion = 6

// maxTrailingNonTurnSkip bounds the walk-back. Measured depth is 0 or 1 and
// never more -- 79 of 239 corpus documents need no skip and 160 need exactly one
// -- so this is margin rather than a working range, and the reported count is
// what makes a deeper history visible instead of silently truncated.
const maxTrailingNonTurnSkip = 4

// trimTrailingNonTurns drops trailing elements that are not conversation turns,
// so the newest element of the stored document is a turn.
//
// Why it exists: the alignment judge sees roughly 390-444 bytes of the document
// and keeps head 3/5 + tail 2/5, so the LAST element is what it reads as the
// current goal. Measured on the live corpus, that element was a non-turn on 67%
// of calls -- usually a 51-132 byte `role:"system"` token counter -- and the
// judged tail therefore carried no turn on two calls in three.
//
// It returns the count as well as the messages, because dropping an element is a
// loss and an unreported loss is the one thing this file does not permit.
func trimTrailingNonTurns(messages []json.RawMessage) ([]json.RawMessage, int) {
	skipped := 0
	for skipped < maxTrailingNonTurnSkip && skipped < len(messages) {
		if isTurn(messages[len(messages)-1-skipped]) {
			break
		}
		skipped++
	}
	if skipped == 0 {
		return messages, 0
	}
	if skipped == len(messages) {
		// Every element is a non-turn, so there is no real turn to promote and
		// dropping them all would store an empty conversation -- worse than
		// storing a synthetic newest element, and the same emptied-history defect
		// keepNewestMessages' bool exists to prevent. Measured at 1% of documents.
		return messages, 0
	}
	return messages[:len(messages)-skipped], skipped
}

// isTurn reports whether an element is a conversation turn, deciding by ROLE and
// never by content.
//
// That is measured, not assumed. The `<total_tokens>` element that keeps landing
// newest carries role "system" -- but the same marker also rides INSIDE real
// turns, on `assistant` 81 times and `user` 59 times across 1,144 occurrences, so
// a content match would destroy 140 genuine turns to catch the synthetic ones.
// Role reaches 98% and cannot match a turn by accident.
//
// Reading role also avoids a hazard this repo has already paid for once:
// `message.content` is a string on user lines and an array on assistant ones,
// which is why it is bound as json.RawMessage rather than a typed slice. Role is
// a scalar on both. The predicate names no provider -- "neither user nor
// assistant" -- so the engine learns no adapter's shape.
//
// An element that will not bind counts as a TURN. Selection may drop what it
// cannot fit; it must never drop what it cannot classify.
func isTurn(message json.RawMessage) bool {
	var probe struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(message, &probe); err != nil {
		return true
	}
	return probe.Role == "user" || probe.Role == "assistant"
}

// tailAsJSONString re-encodes an over-budget message as a marked JSON string. It
// cannot stay a RawMessage: a truncated one is invalid JSON, and the document has
// to parse.
//
// The tail is sized by MEASURING the encode rather than by reserving a margin for
// it, and the fixed margin it replaces was the defect. Re-encoding still expands
// `"` and `\` 2x and a control byte 6x, so an allowance of 16 bytes was outrun by
// any escaping-heavy message, and this function returned an element LARGER than
// the room it was measured against. That failed the document's belt check, and
// the call lost its whole conversation to a window over the raw body. (`<`, `>`
// and `&` expanded here too, until marshalNoHTMLEscape stopped them.)
//
// Sizing for the 6x worst case instead would divide the kept tail by six on every
// ordinary body to serve the pathological one, which is why the ratio is measured
// from the encode that just happened.
func tailAsJSONString(message json.RawMessage, room int) json.RawMessage {
	const note = "[openbox: one message exceeded the selection budget; its tail is kept]"
	// The caller charges this element len(element)+1 -- the comma or the bracket it
	// replaces -- so that is the bound the encode has to come in under.
	limit := room - 1
	tail := string(message)
	// Encoding never SHRINKS, so no byte past a 1:1 encode of this many bytes can
	// possibly survive, whatever it contains. Cutting to that ceiling first is
	// therefore lossless, and it stops round one from encoding a multi-megabyte
	// pasted file in full only to discover it has to lose 99% of it -- a single
	// message is bounded by the 64 MiB body cap, not by the budget.
	if ceiling := limit - len(note) - 2; ceiling > 0 && len(tail) > ceiling {
		tail = trimLeadingPartialRune(tail[len(tail)-ceiling:])
	}
	for range maxTailEncodeRounds {
		encoded, err := marshalNoHTMLEscape(note + tail)
		if err != nil {
			return json.RawMessage(`""`)
		}
		if len(encoded) <= limit {
			return encoded
		}
		if tail == "" {
			break
		}
		next := len(tail) * limit / len(encoded)
		if next >= len(tail) {
			// Rounding, or a limit the note alone cannot meet. Always make
			// progress, or the loop spends its rounds without shrinking anything.
			next = len(tail) - 1
		}
		if next < 0 {
			next = 0
		}
		tail = trimLeadingPartialRune(tail[len(tail)-next:])
	}
	// The rescale measures the whole tail's expansion while the cut takes the
	// END, so a tail whose expanding bytes sit at the end -- a long plain paste
	// finishing in markup -- refines its estimate each round instead of landing
	// it, and can run the rounds out. Returning the note alone there would store a
	// valid, unmarked, budget-fitting document with NO conversation in it, which is
	// a worse answer than the marked fallback this function exists to avoid. So the
	// last step is a cut that cannot fail to fit: at maxEscapeExpansion bytes per
	// input byte, this many input bytes encode to at most the limit, whatever they
	// happen to contain.
	if guaranteed := (limit - len(note) - 2) / maxEscapeExpansion; guaranteed > 0 {
		if len(tail) > guaranteed {
			tail = trimLeadingPartialRune(tail[len(tail)-guaranteed:])
		}
		if encoded, err := marshalNoHTMLEscape(note + tail); err == nil && len(encoded) <= limit {
			return encoded
		}
	}
	// Not even the note fits the room this element was given. Return the note
	// alone and let the belt check window the selected document: that is a
	// bounded, marked answer, where an oversized element is neither.
	encoded, err := marshalNoHTMLEscape(note)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return encoded
}

// cappedField keeps a head window of one non-conversation field. A truncated
// RawMessage would be invalid JSON, so an over-long one is re-encoded as a
// marked string rather than cut in place.
//
// One function for both fields because they need the same rule and had different
// ones: `system` was capped from the start and `model` was trusted, which is how
// an oversized `model` came to cost a call its whole conversation.
func cappedField(raw json.RawMessage, budget int, what string) json.RawMessage {
	if len(raw) == 0 || len(raw) <= budget {
		return raw
	}
	head := trimPartialRune(string(raw[:budget]))
	encoded, err := marshalNoHTMLEscape(fmt.Sprintf("%s%s%d bytes of %s elided]", head, markerPrefix, len(raw)-len(head), what))
	if err != nil {
		return nil
	}
	return encoded
}

// maxDroppedKeyLen and maxDroppedKeyCount bound the note's OWN key list, the one
// field in the selected document with nothing bounding it. The note sizes the
// skeleton, so an unbounded list drives `room` negative and no message fits --
// the failure modelBudget exists to prevent, through a field this file added.
// Measured: one 60 KiB key name stored 49,152 bytes of that name with the turn
// absent; 4,000 ordinary keys do it identically. A key NAME is metadata, so
// bounding it costs nothing selection keeps, and the overflow is reported inside
// the list so a short list is distinguishable from a truncated one.
const (
	maxDroppedKeyLen   = 128
	maxDroppedKeyCount = 64
)

// droppedKeys names every top-level key selection discards, sorted so the stored
// value is deterministic for the same input. `tools` is the big one: it is
// near-constant boilerplate, it was being stored on every call, and dropping it
// is the one change in this phase that REDUCES egress.
func droppedKeys(fields map[string]json.RawMessage) []string {
	var out []string
	for k := range fields {
		switch k {
		case "model", "system", "messages", "openbox_selection":
		default:
			out = append(out, k)
		}
	}
	sort.Strings(out)
	elided := 0
	if len(out) > maxDroppedKeyCount {
		elided = len(out) - maxDroppedKeyCount
		out = out[:maxDroppedKeyCount]
	}
	for i, k := range out {
		if len(k) > maxDroppedKeyLen {
			out[i] = trimPartialRune(k[:maxDroppedKeyLen]) + "..."
		}
	}
	if elided > 0 {
		out = append(out, fmt.Sprintf("%s%d more key(s) elided]", markerPrefix, elided))
	}
	return out
}

// priorNote reads the note a previous selection pass left behind. Without it the
// recovery pass would report the REDACTED document's size as `original_bytes` and
// file the first pass's note under `dropped_keys` -- so the record would
// understate the very loss it exists to report.
func priorNote(raw json.RawMessage) *selectionNote {
	if len(raw) == 0 {
		return nil
	}
	var note selectionNote
	if err := json.Unmarshal(raw, &note); err != nil {
		return nil
	}
	return &note
}

// mergeNote folds this pass's losses into any earlier pass's, so two passes
// produce one cumulative account rather than the second overwriting the first.
// mergeNote takes THIS pass's account as a struct rather than as a positional
// list, and that is a correctness measure, not a style choice: DroppedMessages,
// OriginalBytes and SkippedTrailingNonTurns are three same-typed ints, so a
// transposed argument at a call site would compile in silence and mislabel a
// count in stored evidence. Named fields make that impossible to write.
func mergeNote(prior *selectionNote, this selectionNote) *selectionNote {
	note := &this
	if prior == nil {
		return note
	}
	note.DroppedMessages += prior.DroppedMessages
	note.SkippedTrailingNonTurns += prior.SkippedTrailingNonTurns
	// The first pass saw the real request; this pass only saw the redacted copy of
	// its own output.
	note.OriginalBytes = prior.OriginalBytes
	note.DroppedKeys = unionKeys(prior.DroppedKeys, note.DroppedKeys)
	return note
}

func unionKeys(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, k := range list {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// documentSourcedFallback is deliberately a DIFFERENT string from the one the
// belt check used when it windowed the raw body. Rows stored before this change
// carry the old wording, so an investigation can tell a window over tool
// boilerplate from a window over conversation without dating the row.
const documentSourcedFallback = "the selected document exceeded the budget; the window is over that document"

// fallbackWindow is the tail window plus a marker that names the SELECTOR, so an
// investigation can tell this layer's decision from the client's downstream
// truncationMark. Tail, for the same reason selection keeps the newest messages.
func fallbackWindow(body, why string) string {
	// `]` closes the marker, and rewindowMarkedBody recovers the boundary by
	// finding the FIRST one -- so a reason containing `]` would move the boundary
	// into the reason text and corrupt the marker it re-emits. Enforced here
	// rather than left to the four reason strings happening not to contain one:
	// Go's own JSON syntax errors quote the offending character in brackets
	// (`invalid character ']' looking for ...`), so folding an err.Error() into a
	// reason is an obvious future improvement that would silently break it.
	why = strings.ReplaceAll(why, "]", ")")
	// Two wordings, because the REASON is the same but the CLAIM is not. A body
	// small enough to keep whole was not windowed, and a marker saying it was is
	// the same defect as a note reporting a drop that did not happen -- which this
	// file forbids two functions away. It matters more here than in stored
	// evidence alone: ForGate hands this exact string to the policy engine.
	//
	// Kept deliberately SHORTER than the windowed wording, so the boundary this
	// branch tests is the tighter of the two and the choice cannot be inverted by
	// the marker's own length.
	whole := fmt.Sprintf("%sselection kept this body whole: %s]", markerPrefix, why)
	if len(whole)+len(body) <= selectionBudget {
		return whole + body
	}
	return markedTailWindow(
		fmt.Sprintf("%sselection fell back to a tail window: %s]", markerPrefix, why), body)
}

// markedTailWindow is the one shape every marked window has: the marker, then as
// much of the END as the budget leaves room for. Tail, for the same reason
// selection keeps the newest messages -- the newest bytes are the ones that
// distinguish this call from the last.
//
// Shared by both paths that produce one, because they had already drifted apart
// on the no-room boundary while the cutting rule was supposed to be identical.
func markedTailWindow(marker, payload string) string {
	room := selectionBudget - len(marker)
	if room <= 0 {
		// The marker alone does not fit. It is cut from the INSIDE rather than the
		// end: the note this function's old head cut removed was the closing bracket
		// and the explanation, "leaving a marker whose own claim its truncation had
		// invalidated" -- which is what the cut then did. elideMarkerInterior keeps
		// both ends, so what survives still says what happened.
		return elideMarkerInterior(marker)
	}
	if len(payload) <= room {
		// Whole, so the marker the CALLER chose has to be one that does not claim a
		// cut; fallbackWindow picks between its two wordings on this same boundary.
		return marker + payload
	}
	return marker + trimLeadingPartialRune(payload[len(payload)-room:])
}

// elideMarkerInterior cuts an over-long marker from the middle, keeping its
// prefix and re-closing the bracket a tail cut would have taken.
//
// A marker is not a window: it is one claim, and half a claim is worse than a
// short one. The reachable case is decodeCapturable's marker, which interpolates
// the origin's own Content-Encoding value -- so the oversized part is untrusted
// input sitting between two halves that both have to survive.
func elideMarkerInterior(marker string) string {
	if len(marker) <= selectionBudget {
		return marker
	}
	const closer = "...]"
	keep := selectionBudget - len(closer)
	if keep <= 0 {
		return trimPartialRune(marker[:selectionBudget])
	}
	return trimPartialRune(marker[:keep]) + closer
}

// rewindowMarkedBody re-cuts an already-marked body that grew past the budget.
//
// A marked body is one the selector fell back on, and redaction runs AFTER that:
// a placeholder longer than the secret it replaced grows the bytes, and
// selectModelCallRequest hands a marked body back untouched by design -- marking
// it twice would stack two claims about the same bytes -- so nothing downstream
// re-trimmed it. Measured: 4 stored rows at 49,164-49,232 bytes.
//
// Left alone the only remaining bound is capRunes, and capRunes HEAD-cuts. So an
// over-budget marked body would lose the END of a tail window while the surviving
// head marker still claimed the tail was kept: a marker stating the opposite of
// what happened, which is the original defect of this whole area.
//
// The re-cut cannot be a plain tail cut either, because the marker is at the
// start and a tail cut takes it off, leaving the unmarked window the marker
// exists to prevent. So the marker is kept and the bytes AFTER it are tail-cut --
// the same shape fallbackWindow built, making no second claim.
func rewindowMarkedBody(body string) string {
	if !strings.HasPrefix(body, markerPrefix) {
		// Not a marker, so the selector's own bound already applies and the `]`
		// search below would be cutting at an arbitrary byte of JSON.
		return body
	}
	end := strings.IndexByte(body, ']')
	if end < 0 || end+1 >= len(body) {
		// A marker with nothing after it -- the shape the decode step's own markers
		// have, where the reason names an unusable Content-Encoding and no body
		// follows. It still has to be bounded: measured, a 90,125-byte instance
		// passed through untouched and capRunes then head-cut it to 65,536 bytes
		// with the closing bracket gone.
		return markedTailWindow(body, "")
	}
	return markedTailWindow(body[:end+1], body[end+1:])
}

// trimLeadingPartialRune drops a partial rune at the START, which is where a tail
// cut leaves one. trimPartialRune handles the other end.
func trimLeadingPartialRune(s string) string {
	for i := 0; i < len(s) && i < utf8.UTFMax; i++ {
		if utf8.RuneStart(s[i]) {
			return s[i:]
		}
	}
	return s
}
