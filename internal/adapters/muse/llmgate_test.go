package muse

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

func TestModelCallRequestedMapping(t *testing.T) {
	m := testMapper()
	ev, ok := m.Map(HookPreLLMCall, parseFixture(t, "pre-llm-call"))
	if !ok {
		t.Fatal("PreLLMCall did not map")
	}
	if ev.EventType != client.EventModelCallRequested || ev.ModelCallRequestID != "turn-0001:0:1.1" {
		t.Fatalf("event = %s id %q", ev.EventType, ev.ModelCallRequestID)
	}
	if ev.Model != "muse-spark-1.3-contributor" {
		t.Errorf("model = %q", ev.Model)
	}
	if ev.Metadata["provider"] != "model.meta.response" || ev.Metadata["message_count"] != 2 || ev.Metadata["tool_count"] != 5 {
		t.Errorf("metadata = %v", ev.Metadata)
	}
	names, _ := ev.Metadata["tool_names"].([]string)
	if len(names) != 5 || names[4] != "edit_file" {
		t.Errorf("tool_names = %v", ev.Metadata["tool_names"])
	}
	if _, present := ev.Metadata["message_previews"]; present {
		t.Error("previews carried with capture off")
	}
	if ev.Span != nil || ev.Content != nil || ev.Tokens != nil || ev.TurnIndex != nil {
		t.Errorf("a gate carries no span, content, usage or turn: %+v", ev)
	}
	if ev.ProxyRequestID != "" || ev.GatewayRequestID != "" || ev.OtelRequestID != "" {
		t.Error("a gate must set no lane request id")
	}
}

func TestModelCallPreviewsAreGatedAndRedacted(t *testing.T) {
	secret := "LLM|" + strings.Repeat("7", 12) + "|" + strings.Repeat("a", 24)
	e := parseFixture(t, "pre-llm-call")
	e.Messages = []json.RawMessage{
		json.RawMessage(`{"role":"user","text_preview":"use key ` + secret + ` please"}`),
		json.RawMessage(`{"role":"system","text_preview":""}`),
		json.RawMessage(`"bare string preview"`),
	}
	e.MessageCount = nil
	m := testMapper()
	m.CaptureContent = true
	m.RedactContent = func(s string) string { return strings.ReplaceAll(s, secret, "[scrubbed]") }
	ev, _ := m.Map(HookPreLLMCall, e)
	previews, _ := ev.Metadata["message_previews"].([]string)
	if len(previews) != 2 {
		t.Fatalf("previews = %v", previews)
	}
	for _, p := range previews {
		if strings.Contains(p, secret) {
			t.Errorf("a preview carried the secret: %q", p)
		}
	}
	if previews[0] != "use key [scrubbed] please" {
		t.Errorf("preview 0 = %q", previews[0])
	}
	if ev.Metadata["message_count"] != 3 {
		t.Errorf("message_count falls back to the list length, got %v", ev.Metadata["message_count"])
	}

	m.CaptureContent = false
	ev, _ = m.Map(HookPreLLMCall, e)
	if _, present := ev.Metadata["message_previews"]; present {
		t.Error("capture off carried previews")
	}
}

func TestModelCallRequestID(t *testing.T) {
	base := func() *HookEvent {
		return &HookEvent{SessionID: "s", TurnID: "t", RequestID: "req-9", Attempt: json.RawMessage(`2`), Step: json.RawMessage(`4`)}
	}
	if got := modelCallRequestID(base()); got != "req-9.2" {
		t.Errorf("id = %q", got)
	}
	e := base()
	e.Attempt = nil
	if got := modelCallRequestID(e); got != "req-9.0" {
		t.Errorf("no attempt: id = %q", got)
	}
	// An id the contract cannot carry is replaced by a surrogate that is usable,
	// stable, and the same on the PostLLMCall half.
	for name, bad := range map[string]string{
		"empty": "", "space": "req 9", "long": strings.Repeat("r", 200), "non-ascii": "req-漢",
	} {
		e := base()
		e.RequestID = bad
		id := modelCallRequestID(e)
		if !client.UsableModelCallRequestID(id) {
			t.Errorf("%s: surrogate %q is not usable", name, id)
		}
		if again := modelCallRequestID(e); again != id {
			t.Errorf("%s: surrogate is not stable", name)
		}
		e2 := base()
		e2.RequestID = bad
		e2.Attempt = json.RawMessage(`3`)
		if modelCallRequestID(e2) == id {
			t.Errorf("%s: a retry shares its surrogate with the first attempt", name)
		}
	}
}

func TestModelCallFinishedMapping(t *testing.T) {
	m := testMapper()
	pre, _ := m.Map(HookPreLLMCall, parseFixture(t, "pre-llm-call"))
	post, ok := m.Map(HookPostLLMCall, parseFixture(t, "post-llm-call-tool-calls"))
	if !ok {
		t.Fatal("PostLLMCall did not map")
	}
	if post.ModelCallRequestID != pre.ModelCallRequestID {
		t.Fatalf("the halves name different gates: %q vs %q", pre.ModelCallRequestID, post.ModelCallRequestID)
	}
	if post.EventType != client.EventModelCallFinished || post.Status != client.StatusCompleted {
		t.Errorf("post = %s / %q", post.EventType, post.Status)
	}
	if post.Metadata["finish_reason"] != "tool_calls" || post.Metadata["response_id"] != "resp_0002" || post.Metadata["tool_call_count"] != 1 {
		t.Errorf("metadata = %v", post.Metadata)
	}
	for _, leaked := range []string{"usage", "tokens", "output_text_preview", "input_tokens"} {
		if _, present := post.Metadata[leaked]; present {
			t.Errorf("metadata carries %s", leaked)
		}
	}
	if post.Tokens != nil || post.Content != nil || post.Span != nil {
		t.Errorf("a finished gate carries no usage, content or span: %+v", post)
	}
	if client.WireActivityID(pre) != client.WireActivityID(post) || client.WireActivityID(pre) == "" {
		t.Errorf("wire activity ids %q vs %q", client.WireActivityID(pre), client.WireActivityID(post))
	}
}

func TestModelCallFailureIsClassNotText(t *testing.T) {
	m := testMapper()
	for name, tc := range map[string]struct {
		status, errRaw, wantClass string
		failed                    bool
	}{
		"completed":            {"completed", ``, "", false},
		"no status no error":   {"", ``, "", false},
		"error object":         {"failed", `{"type":"rate_limit","message":"secret text here"}`, "rate_limit", true},
		"error token":          {"", `"timeout"`, "timeout", true},
		"error sentence":       {"", `"connection reset while sending token sk-abc"`, "", true},
		"cancelled":            {"cancelled", ``, "", true},
		"error object no type": {"error", `{"message":"a sentence"}`, "", true},
	} {
		e := parseFixture(t, "post-llm-call")
		e.Status = tc.status
		e.Error = json.RawMessage(tc.errRaw)
		ev, _ := m.Map(HookPostLLMCall, e)
		if (ev.Status == client.StatusFailed) != tc.failed {
			t.Errorf("%s: status = %q", name, ev.Status)
		}
		got, _ := ev.Metadata["error_class"].(string)
		if got != tc.wantClass {
			t.Errorf("%s: error_class = %q, want %q", name, got, tc.wantClass)
		}
		raw, _ := json.Marshal(ev)
		if strings.Contains(string(raw), "secret text") || strings.Contains(string(raw), "sk-abc") {
			t.Errorf("%s: error text reached the event: %s", name, raw)
		}
	}
}

func TestLLMTargetMapsThroughTheObserveMapper(t *testing.T) {
	tg := llmTarget{id: Identity{DeveloperDID: testDID}, mapper: testMapper(), ev: parseFixture(t, "pre-llm-call")}
	ev, ok := tg.DevEvent(nil)
	if !ok || ev.EventType != client.EventModelCallRequested {
		t.Fatalf("DevEvent = %+v %v", ev, ok)
	}
	if tg.ToolInput() != nil || tg.HighRisk() {
		t.Error("a model call has no tool input and is not a high-risk class")
	}
	if req := tg.DecisionRequest(true); req.EventType != client.EventModelCallRequested || req.Content != nil {
		t.Errorf("decision request = %+v", req)
	}
}

// A long conversation's previews are its newest messages, oldest first: the
// tail is what the model call is about to send, so a content policy has to see
// it, not the system prompt and first turns every time.
func TestModelCallPreviewsAreTheNewestMessages(t *testing.T) {
	e := parseFixture(t, "pre-llm-call")
	e.Messages = nil
	for i := 1; i <= maxPreviews+4; i++ {
		e.Messages = append(e.Messages, json.RawMessage(`{"role":"user","text_preview":"msg `+strconv.Itoa(i)+`"}`))
	}
	m := testMapper()
	m.CaptureContent = true
	ev, _ := m.Map(HookPreLLMCall, e)
	previews, _ := ev.Metadata["message_previews"].([]string)
	if len(previews) != maxPreviews {
		t.Fatalf("previews = %d, want %d", len(previews), maxPreviews)
	}
	if previews[0] != "msg 5" || previews[maxPreviews-1] != "msg 20" {
		t.Errorf("previews run %q..%q, want msg 5..msg 20", previews[0], previews[maxPreviews-1])
	}
}
