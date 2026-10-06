package muse

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
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
		t.Error("retired previews key carried")
	}
	if ev.Span != nil || ev.Content != nil || ev.Tokens != nil || ev.TurnIndex != nil {
		t.Errorf("a gate carries no span, content, usage or turn: %+v", ev)
	}
	if ev.ProxyRequestID != "" || ev.GatewayRequestID != "" || ev.OtelRequestID != "" {
		t.Error("a gate must set no lane request id")
	}
}

// The request body is gated content: redacted before it is attached, whole
// rather than cut to a preview.
func TestModelCallRequestBodyIsGatedAndRedacted(t *testing.T) {
	secret := "LLM|" + strings.Repeat("7", 12) + "|" + strings.Repeat("a", 24)
	e := parseFixture(t, "pre-llm-call")
	e.Messages = []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"use key ` + secret + ` please"}`),
		json.RawMessage(`{"role":"system","content":"` + strings.Repeat("long ", 200) + `"}`),
	}
	e.MessageCount = nil
	m := testMapper()
	m.CaptureContent = true
	m.RedactContent = func(s string) string { return strings.ReplaceAll(s, secret, "[scrubbed]") }
	ev, _ := m.Map(HookPreLLMCall, e)
	if ev.Span == nil || ev.Span.RequestBody == "" {
		t.Fatalf("no request body: %+v", ev.Span)
	}
	body := ev.Span.RequestBody
	if strings.Contains(body, secret) || !strings.Contains(body, "use key [scrubbed] please") {
		t.Errorf("body not redacted: %q", body)
	}
	if want := RequestBody(e, m.redact); body != want {
		t.Errorf("body differs from RequestBody")
	}
	// Whole messages, not a 256-rune preview.
	if !strings.Contains(body, strings.Repeat("long ", 200)) {
		t.Error("a long message was truncated")
	}
	var parsed struct {
		Messages []json.RawMessage `json:"messages"`
		Tools    []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil || len(parsed.Messages) != 2 || len(parsed.Tools) != 5 {
		t.Errorf("body shape: %v messages=%d tools=%d", err, len(parsed.Messages), len(parsed.Tools))
	}
	if ev.Metadata["message_count"] != 2 {
		t.Errorf("message_count = %v", ev.Metadata["message_count"])
	}
	if _, present := ev.Metadata["message_previews"]; present {
		t.Error("retired previews key carried")
	}

	m.CaptureContent = false
	ev, _ = m.Map(HookPreLLMCall, e)
	if ev.Span != nil {
		t.Errorf("capture off carried a span: %+v", ev.Span)
	}
}

// The wire, not the struct: through the real hook, the gate's
// activity_input.content is the request body byte for byte (under the cap), and
// is absent with capture off. The fake core refuses a malformed row.
func TestModelCallRequestReachesTheWireAsContent(t *testing.T) {
	setHookEnv(t)
	f := serveCore(t, fakecore.Script{Default: allowJSON})
	t.Setenv(devconfig.EnvContentCapture, "1")
	runHook(t, "PreLLMCall", fixture(t, "pre-llm-call", "s-wire"))
	t.Setenv(devconfig.EnvContentCapture, "0")
	runHook(t, "PreLLMCall", fixture(t, "pre-llm-call", "s-wire-off"))

	want := RequestBody(parseFixture(t, "pre-llm-call"), nil)
	if want == "" {
		t.Fatal("fixture yields no request body")
	}
	var on, off int
	for _, r := range gateRows(f) {
		in, _ := r.Body["activity_input"].(map[string]any)
		content, has := in["content"].(string)
		if strings.Contains(string(r.Raw), "s-wire-off") {
			off++
			if has {
				t.Errorf("capture off: content on the wire: %q", content)
			}
			continue
		}
		on++
		if !has || content != want {
			t.Errorf("wire content = %q, want %q", content, want)
		}
		if _, present := in["message_previews"]; present {
			t.Error("retired previews on the wire")
		}
		if _, present := r.Body["spans"]; present {
			t.Error("spans[] on the wire")
		}
	}
	if on != 1 || off != 1 {
		t.Fatalf("gate rows on=%d off=%d, want 1 each", on, off)
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

// With a request id the contract cannot carry, the surrogate id of a parent's
// call and of a folded child's call with the same turn, step and attempt differ.
func TestFallbackRequestIDSeparatesAFoldedChildFromItsParent(t *testing.T) {
	parent := &HookEvent{SessionID: "sess-p", TurnID: "t1", Step: json.RawMessage(`1`)}
	child := &HookEvent{SessionID: "sess-p", TurnID: "t1", Step: json.RawMessage(`1`), SubagentSessionID: "sess-c"}
	pid, cid := modelCallRequestID(parent), modelCallRequestID(child)
	if !strings.HasPrefix(pid, "h-") || !strings.HasPrefix(cid, "h-") {
		t.Fatalf("expected surrogate ids, got %q and %q", pid, cid)
	}
	if pid == cid {
		t.Errorf("a parent and its folded child share the gate id %q", pid)
	}
}
