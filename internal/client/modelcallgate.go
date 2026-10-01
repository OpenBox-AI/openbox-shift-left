package client

import (
	"encoding/json"
	"fmt"
)

// A model-call gate is a provider's pre-send hook evaluated through policy,
// paired with the hook that reports the call finished. It is not a model-call
// record: lane producers (hook, gateway, telemetry, proxy) describe the call
// itself and carry usage, while a gate describes the decision to send it and
// carries none. Every rule below exists to keep the two from being mistaken for
// one another by core's usage extraction, dedupe and goal alignment.

const (
	// maxModelCallPreviewRunes bounds one short metadata string (a tool name or
	// a provider token).
	maxModelCallPreviewRunes = 256
	// maxModelCallToolNames bounds the tool-name summary.
	maxModelCallToolNames = 64
	// maxModelCallRequestIDLen is the producer request-id bound, declared in the
	// schema as maxLength 128 with pattern ^[\x21-\x7e]+$.
	maxModelCallRequestIDLen = 128

	modelCallPreviewsKey = "message_previews"
)

func isModelCallGate(et EventType) bool {
	return et == EventModelCallRequested || et == EventModelCallFinished
}

// UsableModelCallRequestID reports whether id satisfies the contract's bound on
// model_call_request_id: 1 to 128 printable ASCII characters, none of them
// space. It is the same rule gatewayemit applies to a gateway request id.
func UsableModelCallRequestID(id string) bool {
	if id == "" || len(id) > maxModelCallRequestIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

// modelCallGateActivityID is "<session>:llmgate:<model_call_request_id>", a
// namespace disjoint from "cc-act-" tool ids (no ':'), "<session>:turn:<n>"
// and the ":proxy:", ":otel:", ":gateway:" and ":usage:rollup" shapes.
func modelCallGateActivityID(ev DevEvent) string {
	return ev.SessionID + ":llmgate:" + ev.ModelCallRequestID
}

// normalizeModelCallGate returns ev reduced to what a gate may carry. Dropped
// unconditionally, whatever the adapter set: usage and cost (a gate is not a
// completion), the turn index and every producer request id (a gate is not a
// turn and must not collide with one), and the content. Of the span only the
// started half's RequestBody survives: it is the pending request, carried to
// activity_input.content, and every other span field is dropped. Metadata is
// copied, never mutated in place; the retired message_previews key is removed
// so no producer can put a second, shorter copy of the request on the wire.
func normalizeModelCallGate(ev DevEvent) DevEvent {
	ev.Tokens, ev.Cost, ev.TurnIndex = nil, nil, nil
	ev.Content = nil
	if ev.EventType == EventModelCallRequested && ev.Span != nil && ev.Span.RequestBody != "" {
		ev.Span = &Span{RequestBody: ev.Span.RequestBody}
	} else {
		ev.Span = nil
	}
	ev.ProxyRequestID, ev.GatewayRequestID, ev.OtelRequestID = "", "", ""
	ev.SessionRollup = false
	ev.ActivityType = ActivityTypeModelCallGate

	m := make(map[string]any, len(ev.Metadata))
	for k, v := range ev.Metadata {
		m[k] = v
	}
	delete(m, modelCallPreviewsKey)
	if v, ok := m["tool_names"]; ok {
		if names := boundedStrings(v, maxModelCallToolNames, maxModelCallPreviewRunes); len(names) > 0 {
			m["tool_names"] = names
		} else {
			delete(m, "tool_names")
		}
	}
	for _, k := range []string{"finish_reason", "response_id", "error_class", "provider"} {
		if s, ok := m[k].(string); ok {
			m[k] = truncateRunes(s, maxModelCallPreviewRunes)
		}
	}
	ev.Metadata = m
	return ev
}

// boundedStrings reads v as a list of strings, whether it is still a []string
// or came back from the spool as []any, dropping anything that is not a
// non-empty string and cutting each to maxRunes and the list to maxItems.
func boundedStrings(v any, maxItems, maxRunes int) []string {
	var in []string
	switch t := v.(type) {
	case []string:
		in = t
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok {
				in = append(in, s)
			}
		}
	}
	var out []string
	for _, s := range in {
		if s == "" {
			continue
		}
		if len(out) == maxItems {
			break
		}
		out = append(out, truncateRunes(s, maxRunes))
	}
	return out
}

func truncateRunes(s string, n int) string {
	if len(s) <= n { // byte length within the bound implies rune count within it
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// modelCallGateInput is the ActivityStarted input: what a policy matches a
// pre-send decision on. content is the pending request, gated and tail-kept in
// bytes (the newest turn is at the end); it is absent when content capture is
// off or the producer set none, and every other key is structural.
func modelCallGateInput(ev DevEvent, cut *cutLog) json.RawMessage {
	m := map[string]any{}
	if ev.Model != "" {
		m["model"] = ev.Model
	}
	for _, k := range []string{"provider", "message_count", "tool_count", "tool_names"} {
		if v, ok := ev.Metadata[k]; ok {
			m[k] = v
		}
	}
	if ev.Span != nil && ev.Span.RequestBody != "" && !ev.contentStripped {
		if len(ev.Span.RequestBody) > maxModelCallBodyBytes {
			cut.note("activity_input", modelCallContentKey)
		}
		m[modelCallContentKey] = capModelCallRequest(ev.Span.RequestBody)
	}
	return marshalOrNil(m)
}

// modelCallGateOutput is the ActivityCompleted output: metadata only, never the
// reply, thinking or a response body, and never usage.
func modelCallGateOutput(ev DevEvent) json.RawMessage {
	m := map[string]any{}
	if s := statusFor(ev); s != "" {
		m["status"] = s
	}
	if ev.Model != "" {
		m["model"] = ev.Model
	}
	for _, k := range []string{"provider", "finish_reason", "response_id", "error_class", "tool_call_count"} {
		if v, ok := ev.Metadata[k]; ok {
			m[k] = v
		}
	}
	return marshalOrNil(m)
}

// checkModelCallGate refuses a gate event that cannot be placed on a row: an
// unusable id would otherwise reach a stored activity_id verbatim.
func checkModelCallGate(ev DevEvent) error {
	if !UsableModelCallRequestID(ev.ModelCallRequestID) {
		return fmt.Errorf("client: %s needs a model_call_request_id of 1-%d printable ASCII characters", ev.EventType, maxModelCallRequestIDLen)
	}
	return nil
}
