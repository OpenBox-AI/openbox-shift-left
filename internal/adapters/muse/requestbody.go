package muse

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// requestBodyOverhead reserves room for the {"messages":[],"tools":[]} wrapper
// and separators, so the finished body stays within client.MaxModelCallBodyBytes.
const requestBodyOverhead = 64

// RequestBody renders a PreLLMCall/PostLLMCall event's request as the compact
// JSON object {"tools":[...],"messages":[...]} (Muse's session journal holds
// no request, so the hook is the only source). Tools come first because the
// client's request cap keeps the tail of a body: messages last means the newest
// one is what survives any later cut.
//
// The body is bounded to client.MaxModelCallBodyBytes, the client's own
// model-call request cap, the way that cap keeps a conversation, by its tail: messages are taken newest
// first until the budget is spent and then emitted in their original order, so
// the result is always valid JSON and always holds the latest turn. A tool
// list takes at most a quarter of the budget, in order. A newest message that
// alone exceeds the budget is kept as one text message holding its tail. redact
// runs over the serialized string (nil leaves it as is); an empty event gives
// "".
func RequestBody(e *HookEvent, redact func(string) string) string {
	if e == nil || (len(e.Messages) == 0 && len(e.Tools) == 0) {
		return ""
	}
	budget := client.MaxModelCallBodyBytes - requestBodyOverhead

	tools, used := make([]json.RawMessage, 0, len(e.Tools)), 0
	for _, t := range e.Tools {
		if len(t) == 0 || used+len(t)+1 > budget/4 {
			break
		}
		tools = append(tools, t)
		used += len(t) + 1
	}

	msgBudget := budget - used
	kept, spent := 0, 0
	for i := len(e.Messages) - 1; i >= 0; i-- {
		n := len(e.Messages[i]) + 1
		if len(e.Messages[i]) == 0 {
			continue
		}
		if spent+n > msgBudget {
			break
		}
		spent += n
		kept++
	}
	var msgs []json.RawMessage
	switch {
	case kept > 0:
		// Count back from the end over non-empty messages, as above.
		msgs = tailMessages(e.Messages, kept)
	case len(e.Messages) > 0:
		if m, ok := squeezeNewest(e.Messages, msgBudget, redact); ok {
			msgs = []json.RawMessage{m}
		}
	}

	body, err := marshalPlain(struct {
		Tools    []json.RawMessage `json:"tools"`
		Messages []json.RawMessage `json:"messages"`
	}{nonNil(tools), nonNil(msgs)})
	if err != nil {
		// A message that is not valid JSON: content-free, nothing is stashed.
		trace.Emit(trace.Record{
			Provider:  provider,
			SessionID: e.SessionID,
			Stage:     trace.StageCapture,
			Outcome:   "muse.request_body",
			Detail:    map[string]any{"reason": "invalid_message"},
		})
		return ""
	}
	out := string(body)
	if redact != nil {
		out = redact(out)
	}
	return out
}

// marshalPlain is json.Marshal without HTML escaping, so the stashed body reads
// as the request did.
func marshalPlain(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func nonNil(in []json.RawMessage) []json.RawMessage {
	if in == nil {
		return []json.RawMessage{}
	}
	return in
}

// tailMessages returns the last n non-empty messages, oldest first.
func tailMessages(all []json.RawMessage, n int) []json.RawMessage {
	out := make([]json.RawMessage, 0, n)
	for i := len(all) - 1; i >= 0 && len(out) < n; i-- {
		if len(all[i]) > 0 {
			out = append(out, all[i])
		}
	}
	for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
		out[l], out[r] = out[r], out[l]
	}
	return out
}

// squeezeNewest rebuilds the newest message as {"role":..,"content":[{"type":
// "text","text":<tail of its JSON>}]} sized to fit budget. JSON string escaping
// can grow the tail, so the cut is halved until the marshalled message fits.
// The message is redacted before its head is cut, so a secret's label is never
// dropped while its value survives; redaction reads a bounded tail (a few times
// the budget) to keep its cost sane.
func squeezeNewest(all []json.RawMessage, budget int, redact func(string) string) (json.RawMessage, bool) {
	var raw json.RawMessage
	for i := len(all) - 1; i >= 0; i-- {
		if len(all[i]) > 0 {
			raw = all[i]
			break
		}
	}
	if raw == nil || budget <= 0 {
		return nil, false
	}
	var head struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(raw, &head) // an unreadable role just leaves it empty
	text := tailBytes(string(raw), budget*4)
	if redact != nil {
		text = redact(text)
	}
	for cut := budget / 2; cut > 0; cut /= 2 {
		b, err := marshalPlain(map[string]any{
			"role":    head.Role,
			"content": []map[string]string{{"type": "text", "text": tailBytes(text, cut)}},
		})
		if err == nil && len(b) <= budget {
			return b, true
		}
	}
	return nil, false
}

// tailBytes keeps the last n bytes of s, starting on a rune boundary.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}
