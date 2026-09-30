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

// traceparentOf reads options.meta.traceparent, and only a well-formed W3C
// value: anything else is not a trace id and is not recorded.
func traceparentOf(options json.RawMessage) string {
	var o struct {
		Meta struct {
			Traceparent string `json:"traceparent"`
		} `json:"meta"`
	}
	if json.Unmarshal(options, &o) != nil || !traceparentRE.MatchString(o.Meta.Traceparent) {
		return ""
	}
	return o.Meta.Traceparent
}

// Outcomes of the local records below, on the capture stage.
const (
	outcomeUsage       = "model_call.usage"
	outcomeTraceparent = "model_call.traceparent"
)

// traceModelCall records a model call's local-only facts, joined to its gate
// row by activity id: the trace context on PreLLMCall, usage on PostLLMCall.
func traceModelCall(hook HookName, e *HookEvent, ev client.DevEvent) {
	rec := trace.Record{
		Provider:   provider,
		SessionID:  e.SessionID,
		RunID:      ev.RunID,
		ActivityID: client.WireActivityID(ev),
		EventID:    ev.EventID,
		EventType:  string(hook),
		Stage:      trace.StageCapture,
	}
	switch hook {
	case HookPreLLMCall:
		tp := traceparentOf(e.Options)
		if tp == "" {
			return
		}
		rec.Outcome = outcomeTraceparent
		rec.Detail = map[string]any{"traceparent": tp}
	case HookPostLLMCall:
		u := usageNumbers(e.Usage)
		if u == nil {
			return
		}
		rec.Outcome = outcomeUsage
		rec.Detail = map[string]any{"usage": u}
	default:
		return
	}
	trace.Emit(rec)
}
