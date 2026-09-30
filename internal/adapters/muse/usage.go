package muse

import (
	"encoding/json"
	"regexp"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// What a model call cost, and the W3C trace context it ran under, are local
// facts. PostLLMCall's usage and PreLLMCall's options.meta.traceparent go to
// the local trace and nowhere else: never onto a DevEvent (a model-call gate
// carries no usage, and the client drops it if one is set), and never onto the
// wire. The trace never egresses.

// usageKeys is the allowlist of usage fields the trace keeps, each read as a
// non-negative integer. A field not named here is dropped, so a payload that
// grows a text field cannot carry content into a record that reads as numbers.
var usageKeys = map[string]bool{
	"input_tokens":       true,
	"output_tokens":      true,
	"cached_tokens":      true,
	"cache_read_tokens":  true,
	"cache_write_tokens": true,
	"reasoning_tokens":   true,
	"total_tokens":       true,
}

// usageNumbers projects a usage object onto usageKeys.
func usageNumbers(raw json.RawMessage) map[string]int {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	out := map[string]int{}
	for k, v := range m {
		if !usageKeys[k] {
			continue
		}
		if n, ok := intOf(v); ok {
			out[k] = n
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var traceparentRE = regexp.MustCompile(`^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)

// traceparentOf reads the W3C trace context from a model call's options, and
// only a well-formed value: anything else is not a trace id and is not
// recorded. Muse 1.4.1 delivers options as a flat map with dotted keys
// ("meta.traceparent"); the nested form is read as a fallback. Only PostLLMCall
// carried it on 1.4.1.
func traceparentOf(options json.RawMessage) string {
	var flat map[string]json.RawMessage
	if json.Unmarshal(options, &flat) != nil {
		return ""
	}
	if tp := str(flat["meta.traceparent"]); traceparentRE.MatchString(tp) {
		return tp
	}
	var nested struct {
		Traceparent string `json:"traceparent"`
	}
	if json.Unmarshal(flat["meta"], &nested) == nil && traceparentRE.MatchString(nested.Traceparent) {
		return nested.Traceparent
	}
	return ""
}

// Outcomes of the local records below, on the capture stage.
const (
	outcomeUsage       = "model_call.usage"
	outcomeTraceparent = "model_call.traceparent"
)

// traceModelCall records a model call's local-only facts, joined to its gate
// row by activity id: the trace context on whichever half carries it, usage on
// PostLLMCall.
func traceModelCall(hook HookName, e *HookEvent, ev client.DevEvent) {
	if hook != HookPreLLMCall && hook != HookPostLLMCall {
		return
	}
	rec := trace.Record{
		Provider:   provider,
		SessionID:  e.SessionID,
		RunID:      ev.RunID,
		ActivityID: client.WireActivityID(ev),
		EventID:    ev.EventID,
		EventType:  string(hook),
		Stage:      trace.StageCapture,
	}
	if tp := traceparentOf(e.Options); tp != "" {
		r := rec
		r.Outcome = outcomeTraceparent
		r.Detail = map[string]any{"traceparent": tp}
		trace.Emit(r)
	}
	if hook == HookPostLLMCall {
		if u := usageNumbers(e.Usage); u != nil {
			r := rec
			r.Outcome = outcomeUsage
			r.Detail = map[string]any{"usage": u}
			trace.Emit(r)
		}
	}
}
