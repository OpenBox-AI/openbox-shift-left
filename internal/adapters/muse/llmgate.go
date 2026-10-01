package muse

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// A Muse model call is a gate, not a model-call record. PreLLMCall opens a
// model-call gate activity that is evaluated like a tool call, before the
// request is sent and on the same gating ceiling; PostLLMCall closes it with
// metadata only. Neither half carries usage, a turn index or a lane producer
// id (the client drops them whatever is set here), so the row can never be read
// as an llm_completion and core's usage extraction and turn counts cannot pick
// it up. Usage stays in the local trace (usage.go), and a call that never
// reports finished (a crash, a cancel, an interrupt) leaves the started row
// only: a completion is never fabricated.

const modelCallToolKind = "model_call"

const (
	maxPreviews  = 16
	maxToolNames = 64
)

// modelCallRequestID names one gate: Muse's request id, a ".", and the attempt,
// so a retry of one request is a new gate. It is what pairs PostLLMCall to its
// PreLLMCall across two processes.
//
// An id the contract cannot carry (empty, over 128 characters, or outside
// printable ASCII) would make the client refuse the event and so deny every
// model call, so a stable surrogate is derived from the keys both halves share.
// The surrogate invents no identity; it is a hash of what Muse sent.
func modelCallRequestID(e *HookEvent) string {
	attempt := "0"
	if n, ok := intOf(e.Attempt); ok {
		attempt = strconv.Itoa(n)
	}
	if id := e.RequestID + "." + attempt; e.RequestID != "" && client.UsableModelCallRequestID(id) {
		return id
	}
	parts := []string{e.SessionID, e.TurnID, str(e.Step), e.RequestID, attempt}
	// A folded child shares its parent's session id, and a turn id and step
	// that match the parent's would collide: the child's own id keeps them
	// apart. Left out when empty so an unfolded call keeps its existing id.
	if e.SubagentSessionID != "" {
		parts = append(parts, e.SubagentSessionID)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return "h-" + hex.EncodeToString(sum[:16])
}

func modelProvider(e *HookEvent) string {
	if e.Provider != "" {
		return capStr(e.Provider)
	}
	return capStr(e.ModelProvider)
}

// count reads a count key, falling back to the length of the list beside it.
func count(raw json.RawMessage, list []json.RawMessage) (int, bool) {
	if n, ok := intOf(raw); ok {
		return n, true
	}
	if list != nil {
		return len(list), true
	}
	return 0, false
}

// nameOf reads one element of tools[]: an object's name, or a bare string.
func nameOf(raw json.RawMessage) string {
	if s := str(raw); s != "" {
		return s
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	return str(m["name"])
}

// previewSourceBytes bounds the text one message contributes before redaction.
// The client cuts a preview to 256 runes, so nothing past this reaches the
// wire, and a secret straddling the bound is never part of what is sent.
const previewSourceBytes = 8 << 10

// previewOf reads one element of messages[]: the text of its content blocks
// ({"type":"text","text":...}), joined, or a bare string content or a
// text_preview. Blocks of any other type (images, tool calls) contribute
// nothing.
func previewOf(raw json.RawMessage) string {
	if s := str(raw); s != "" {
		return hookflow.TruncateBytes(s, previewSourceBytes)
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	if s := str(m["text_preview"]); s != "" {
		return hookflow.TruncateBytes(s, previewSourceBytes)
	}
	if s := str(m["content"]); s != "" && strings.TrimSpace(string(m["content"]))[0] == '"' {
		return hookflow.TruncateBytes(s, previewSourceBytes)
	}
	var blocks []struct {
		Type string          `json:"type"`
		Text json.RawMessage `json:"text"`
	}
	if json.Unmarshal(m["content"], &blocks) != nil {
		return ""
	}
	var parts []string
	size := 0
	for _, b := range blocks {
		if b.Type != "text" {
			continue
		}
		t := str(b.Text)
		if t == "" {
			continue
		}
		parts = append(parts, t)
		if size += len(t); size >= previewSourceBytes {
			break
		}
	}
	return hookflow.TruncateBytes(strings.Join(parts, "\n"), previewSourceBytes)
}

func (m Mapper) mapModelCallRequested(ev client.DevEvent, agent client.Tool, e *HookEvent) (client.DevEvent, bool) {
	ev.EventType = client.EventModelCallRequested
	ev.Tool = agent
	ev.StartedAt = ev.Timestamp
	ev.Model = capStr(e.Model)
	ev.ModelCallRequestID = modelCallRequestID(e)

	meta := hookflow.Compact(map[string]any{"provider": modelProvider(e)})
	if n, ok := count(e.MessageCount, e.Messages); ok {
		meta["message_count"] = n
	}
	if n, ok := count(e.ToolCount, e.Tools); ok {
		meta["tool_count"] = n
	}
	var names []string
	for _, raw := range e.Tools {
		if len(names) == maxToolNames {
			break
		}
		if n := nameOf(raw); n != "" {
			names = append(names, capStr(n))
		}
	}
	if len(names) > 0 {
		meta["tool_names"] = names
	}
	// The previews are content: behind the capture gate, redacted before they
	// are attached. The client cuts each to 256 runes and strips them when
	// capture is off. They are the NEWEST messages, in conversation order: the
	// tail is what is about to be sent, and the head (the system prompt, the
	// first turns) would fill the quota with the same text on every call.
	if m.CaptureContent {
		var previews []string
		for i := len(e.Messages) - 1; i >= 0 && len(previews) < maxPreviews; i-- {
			if p := m.redact(previewOf(e.Messages[i])); p != "" {
				previews = append(previews, p)
			}
		}
		slices.Reverse(previews)
		if len(previews) > 0 {
			meta["message_previews"] = previews
		}
	}
	ev.Metadata = meta
	ev.EventID = m.eventID(ev)
	return ev, true
}

// completedStatuses are the PostLLMCall status values that read as a success;
// any other non-empty status is a failure. Observed on 1.4.1: "success" (a
// text answer) and "tool_calls" (the model asked for tools), both with a null
// error. The others are kept for spellings not yet seen.
var completedStatuses = map[string]bool{
	"completed": true, "complete": true, "ok": true, "success": true, "succeeded": true, "tool_calls": true,
}

func (m Mapper) mapModelCallFinished(ev client.DevEvent, agent client.Tool, e *HookEvent) (client.DevEvent, bool) {
	ev.EventType = client.EventModelCallFinished
	ev.Tool = agent
	ev.EndedAt = ev.Timestamp
	ev.Model = capStr(e.Model)
	ev.ModelCallRequestID = modelCallRequestID(e)

	errClass := errorClass(e.Error)
	failed := (e.Status != "" && !completedStatuses[strings.ToLower(e.Status)]) || hasValue(e.Error)
	ev.Status = client.StatusCompleted
	if failed {
		ev.Status = client.StatusFailed
	}

	meta := hookflow.Compact(map[string]any{
		"provider":      modelProvider(e),
		"finish_reason": capStr(e.FinishReason),
		"response_id":   capStr(e.ResponseID),
		"error_class":   errClass,
	})
	if n, ok := intOf(e.ToolCallCount); ok {
		meta["tool_call_count"] = n
	}
	ev.Metadata = meta
	ev.EventID = m.eventID(ev)
	return ev, true
}

func hasValue(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != `""` && s != "{}"
}

var classToken = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// errorClass is the class of a failure, never its text: an error object's
// class/type/code/name when it is a short token, or a bare string only when it
// is itself one. A sentence, which could quote content, reads as no class.
func errorClass(raw json.RawMessage) string {
	if !hasValue(raw) {
		return ""
	}
	if s := str(raw); s != "" {
		if classToken.MatchString(s) {
			return s
		}
		return ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range []string{"class", "type", "code", "name"} {
		if s := str(m[k]); classToken.MatchString(s) {
			return s
		}
	}
	return ""
}

// threadModelCallDuration bridges a model-call gate's start time from its
// PreLLMCall process to its PostLLMCall process, as hookflow's ThreadDuration
// does for a tool call (which it handles for tool events only): the started
// half stashes its start under the gate's request id, and the finished half
// adopts it, which the client turns into the completed row's duration_ms. A
// finished half whose started half never recorded one keeps no duration.
func threadModelCallDuration(d hookflow.DurationStash, ev *client.DevEvent) {
	switch ev.EventType {
	case client.EventModelCallRequested:
		_ = d.PutModelCallStart(ev.SessionID, ev.ModelCallRequestID, ev.StartedAt)
	case client.EventModelCallFinished:
		if started := d.TakeModelCallStart(ev.SessionID, ev.ModelCallRequestID); started != "" {
			ev.StartedAt = started
		}
	}
}

// llmTarget is the gate's view of a PreLLMCall.
type llmTarget struct {
	id     Identity
	mapper Mapper
	ev     *HookEvent
}

func (t llmTarget) SessionID() string { return t.ev.SessionID }

// ToolName labels the gate's diagnostics and the pending-approval marker.
func (t llmTarget) ToolName() string { return modelCallToolKind }

// ToolInput: a model call has no tool_input, so nothing could be rewritten.
func (t llmTarget) ToolInput() json.RawMessage { return nil }

func (t llmTarget) HighRisk() bool { return false }

// DecisionRequest carries identity axes only; the verdict is /evaluate's.
func (t llmTarget) DecisionRequest(bool) decision.DecisionRequest {
	return decision.DecisionRequest{
		SessionID:    t.ev.SessionID,
		DeveloperDID: t.id.DeveloperDID,
		EventType:    client.EventModelCallRequested,
		Tool:         client.Tool{Name: agentToolName, Kind: client.ToolShell},
	}
}

// DevEvent maps the call through the same Mapper (and pinned clock) the observe
// copy uses, so the two derive one event_id and the one gate activity id.
func (t llmTarget) DevEvent(*client.Content) (client.DevEvent, bool) {
	return t.mapper.Map(HookPreLLMCall, t.ev)
}

var _ hookflow.EnforceTarget = llmTarget{}

func recordModelCallEnforcement(logger *log.Logger, e *HookEvent, dec decision.Decision, res hookflow.ApplyResult) {
	hookflow.RecordEnforcement(logger, e.SessionID, modelCallToolKind, dec, res)
}
