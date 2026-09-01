package gateway

import (
	"encoding/json"
	"fmt"
	"sort"
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
// 48 KiB leaves 16 KiB of redaction headroom, about 33%. The residual case is a
// body redaction grows by more than that, which means hundreds of detected
// secrets in one prompt; there capRunes still head-cuts and the note at the head
// survives while the newest messages do not.
// TestRedactionGrowthDoesNotCostTheNewestTurn pins the headroom against a
// deliberately secret-dense body rather than leaving it as arithmetic.
const selectionBudget = 48 * 1024

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
	DroppedMessages int      `json:"dropped_messages"`
	DroppedKeys     []string `json:"dropped_keys,omitempty"`
	OriginalBytes   int      `json:"original_bytes"`
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
		return fallbackWindow(body, "the body is not a JSON object")
	}
	rawMessages, ok := fields["messages"]
	if !ok {
		return fallbackWindow(body, "the body carries no messages array")
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(rawMessages, &messages); err != nil {
		return fallbackWindow(body, "messages is not an array")
	}

	doc := selectedRequest{
		Model:  fields["model"],
		System: cappedSystem(fields["system"]),
	}
	dropped := droppedKeys(fields)

	// Overhead first, so the message budget is what is actually left rather than
	// an estimate. Marshalling the document with an empty history costs one small
	// allocation and removes the guesswork.
	kept, note := keepNewestMessages(&doc, messages, len(body), dropped)
	doc.Messages = kept
	doc.Selection = note

	out, err := json.Marshal(doc)
	if err != nil {
		// Unreachable in practice: every field is either a validated RawMessage or
		// a plain scalar. Falling back rather than panicking keeps the relay's one
		// hard rule -- never break the tool -- ahead of this function's ambitions.
		return fallbackWindow(body, "the selected document could not be encoded")
	}
	if len(out) > selectionBudget {
		// Belt: keepNewestMessages already fits the budget, so reaching here means
		// the overhead alone exceeds it, which only a pathological `model` value can
		// do. A tail window is still better evidence than a truncated JSON document.
		return fallbackWindow(body, "the selected document exceeded the budget")
	}
	return string(out)
}

// keepNewestMessages fills the budget greedily FROM THE END. Newest-first is the
// entire ordering argument of this phase: the last entry is the turn that
// distinguishes this call from the previous one.
func keepNewestMessages(doc *selectedRequest, messages []json.RawMessage, originalBytes int, dropped []string) ([]json.RawMessage, *selectionNote) {
	// An empty history is a complete document, not a truncated one.
	if len(messages) == 0 {
		return []json.RawMessage{}, nil
	}

	probe := *doc
	probe.Messages = []json.RawMessage{}
	probe.Selection = &selectionNote{
		DroppedMessages: len(messages),
		DroppedKeys:     dropped,
		OriginalBytes:   originalBytes,
	}
	skeleton, err := json.Marshal(probe)
	if err != nil {
		return nil, nil
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

	if len(keep) == 0 {
		// One message larger than the whole budget: a pasted file can be the entire
		// history. Storing an empty conversation for the call that most needs
		// evidence is the worse answer, so keep that message's TAIL as a string.
		keep = append(keep, tailAsJSONString(messages[len(messages)-1], room))
	}

	if len(keep) == len(messages) && len(dropped) == 0 {
		// Nothing was dropped, so a note would claim a loss that did not happen --
		// the same class of defect as a marker claiming a cut that did not happen.
		return keep, nil
	}
	return keep, &selectionNote{
		DroppedMessages: len(messages) - len(keep),
		DroppedKeys:     dropped,
		OriginalBytes:   originalBytes,
	}
}

// tailAsJSONString re-encodes an over-budget message as a marked JSON string. It
// cannot stay a RawMessage: a truncated one is invalid JSON, and the document has
// to parse.
func tailAsJSONString(message json.RawMessage, room int) json.RawMessage {
	const note = "[openbox: one message exceeded the selection budget; its tail is kept]"
	room -= len(note) + 16 // the quoting and escaping the re-encode will add
	if room < 0 {
		room = 0
	}
	tail := string(message)
	if len(tail) > room {
		tail = trimLeadingPartialRune(tail[len(tail)-room:])
	}
	encoded, err := json.Marshal(note + tail)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return encoded
}

// cappedSystem keeps a head window of the system prompt. A truncated
// RawMessage would be invalid JSON, so an over-long one is re-encoded as a
// marked string rather than cut in place.
func cappedSystem(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || len(raw) <= systemBudget {
		return raw
	}
	head := trimPartialRune(string(raw[:systemBudget]))
	encoded, err := json.Marshal(fmt.Sprintf("%s%s%d bytes of system prompt elided]", head, markerPrefix, len(raw)-len(head)))
	if err != nil {
		return nil
	}
	return encoded
}

// droppedKeys names every top-level key selection discards, sorted so the stored
// value is deterministic for the same input. `tools` is the big one: it is
// near-constant boilerplate, it was being stored on every call, and dropping it
// is the one change in this phase that REDUCES egress.
func droppedKeys(fields map[string]json.RawMessage) []string {
	var out []string
	for k := range fields {
		switch k {
		case "model", "system", "messages":
		default:
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// fallbackWindow is the tail window plus a marker that names the SELECTOR, so an
// investigation can tell this layer's decision from the client's downstream
// truncationMark. Tail, for the same reason selection keeps the newest messages.
func fallbackWindow(body, why string) string {
	marker := fmt.Sprintf("%sselection fell back to a tail window: %s]", markerPrefix, why)
	room := selectionBudget - len(marker)
	if room < 0 {
		return marker
	}
	if len(body) <= room {
		return marker + body
	}
	return marker + trimLeadingPartialRune(body[len(body)-room:])
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
