package muse

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

func msgJSON(role, text string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"role": role, "content": []map[string]string{{"type": "text", "text": text}}})
	return b
}

type bodyDoc struct {
	Messages []struct {
		Role    string `json:"role"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"messages"`
	Tools []json.RawMessage `json:"tools"`
}

func parseBody(t *testing.T, s string) bodyDoc {
	t.Helper()
	var d bodyDoc
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		t.Fatalf("not valid JSON (%v): %.200s", err, s)
	}
	return d
}

func TestRequestBodyFromRealShapedHook(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "post-llm-call.json"))
	if err != nil {
		t.Fatal(err)
	}
	var e HookEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	if len(e.Messages) == 0 {
		t.Fatal("fixture has no messages")
	}
	got := RequestBody(&e, nil)
	d := parseBody(t, got)
	if len(d.Messages) != len(e.Messages) || len(d.Tools) != len(e.Tools) {
		t.Errorf("messages %d/%d tools %d/%d", len(d.Messages), len(e.Messages), len(d.Tools), len(e.Tools))
	}
	if !strings.HasPrefix(got, `{"tools":[`) || strings.Contains(got, "\n") {
		t.Errorf("not compact tools-first JSON: %.120s", got)
	}
}

func TestRequestBodyRedactsTheSerializedString(t *testing.T) {
	e := &HookEvent{Messages: []json.RawMessage{msgJSON("user", "token SECRET here")}, Tools: []json.RawMessage{json.RawMessage(`{"name":"SECRET-tool"}`)}}
	got := RequestBody(e, func(s string) string { return strings.ReplaceAll(s, "SECRET", "[R]") })
	if strings.Contains(got, "SECRET") || !strings.Contains(got, "[R]") {
		t.Fatalf("redaction not applied: %s", got)
	}
	parseBody(t, got)
}

func TestRequestBodyEmptyIsEmpty(t *testing.T) {
	if RequestBody(nil, nil) != "" || RequestBody(&HookEvent{}, nil) != "" {
		t.Error("an event with no messages or tools must give no body")
	}
}

func TestRequestBodyOverBudgetKeepsNewestInOrder(t *testing.T) {
	chunk := strings.Repeat("a", 20<<10)
	var msgs []json.RawMessage
	for i := 0; i < 10; i++ { // ~200 KiB, three times the cap
		msgs = append(msgs, msgJSON("user", fmt.Sprintf("m%02d:%s", i, chunk)))
	}
	got := RequestBody(&HookEvent{Messages: msgs}, nil)
	if len(got) > client.MaxModelCallBodyBytes {
		t.Fatalf("body = %d bytes, over %d", len(got), client.MaxModelCallBodyBytes)
	}
	d := parseBody(t, got)
	if len(d.Messages) == 0 || len(d.Messages) >= 10 {
		t.Fatalf("kept %d of 10", len(d.Messages))
	}
	last := d.Messages[len(d.Messages)-1].Content[0].Text
	if !strings.HasPrefix(last, "m09:") {
		t.Errorf("newest message dropped, last = %.8s", last)
	}
	prev := -1
	for _, m := range d.Messages {
		var n int
		fmt.Sscanf(m.Content[0].Text, "m%02d:", &n)
		if n <= prev {
			t.Fatalf("messages out of order: %d after %d", n, prev)
		}
		prev = n
	}
	if d.Messages[0].Content[0].Text[:4] == "m00:" {
		t.Error("oldest message kept over the budget")
	}
}

func TestRequestBodySingleOversizeMessageStaysValidAndKeptTail(t *testing.T) {
	e := &HookEvent{Messages: []json.RawMessage{msgJSON("user", strings.Repeat("b", client.MaxModelCallBodyBytes*2))}}
	got := RequestBody(e, nil)
	if len(got) > client.MaxModelCallBodyBytes {
		t.Fatalf("body = %d bytes", len(got))
	}
	d := parseBody(t, got)
	if len(d.Messages) != 1 || d.Messages[0].Role != "user" || d.Messages[0].Content[0].Text == "" {
		t.Errorf("newest message lost: %+v", d.Messages)
	}
}

func TestRequestBodyBoundIsInBytesForCJK(t *testing.T) {
	var msgs []json.RawMessage
	for i := 0; i < 6; i++ {
		msgs = append(msgs, msgJSON("user", strings.Repeat("漢", 60<<10))) // 180 KiB each in bytes, 60Ki runes
	}
	got := RequestBody(&HookEvent{Messages: msgs}, nil)
	if len(got) > client.MaxModelCallBodyBytes {
		t.Fatalf("body = %d bytes, over the byte cap", len(got))
	}
	if n := len(parseBody(t, got).Messages); n < 1 || n > 3 {
		t.Errorf("kept %d messages, want 1..3", n)
	}
}

func TestRequestBodyBoundsTools(t *testing.T) {
	var tools []json.RawMessage
	for i := 0; i < 20; i++ {
		tools = append(tools, json.RawMessage(fmt.Sprintf(`{"name":"t%d","description":"%s"}`, i, strings.Repeat("d", 5<<10))))
	}
	got := RequestBody(&HookEvent{Messages: []json.RawMessage{msgJSON("user", "hi")}, Tools: tools}, nil)
	if len(got) > client.MaxModelCallBodyBytes {
		t.Fatalf("body = %d bytes", len(got))
	}
	d := parseBody(t, got)
	if len(d.Messages) != 1 || len(d.Tools) == 0 || len(d.Tools) >= 20 {
		t.Errorf("messages %d tools %d", len(d.Messages), len(d.Tools))
	}
}

// The cap is the client's own model-call request cap, so the stashed body is
// never cut a second time on its way out.
func TestRequestBodyEmitsToolsBeforeMessagesSoTailKeepingKeepsTheNewestMessage(t *testing.T) {
	e := &HookEvent{
		Messages: []json.RawMessage{msgJSON("user", "newest-marker")},
		Tools:    []json.RawMessage{json.RawMessage(`{"name":"t"}`)},
	}
	got := RequestBody(e, nil)
	if strings.Index(got, `"tools"`) > strings.Index(got, `"messages"`) {
		t.Errorf("tools after messages: %s", got)
	}
}

func TestRequestBodyRedactsBeforeCuttingAnOversizeNewestMessage(t *testing.T) {
	redact := func(s string) string {
		return regexp.MustCompile(`api_key=\S+`).ReplaceAllString(s, "api_key=[R]")
	}
	e := &HookEvent{Messages: []json.RawMessage{msgJSON("user", "api_key="+strings.Repeat("S", client.MaxModelCallBodyBytes*2))}}
	got := RequestBody(e, redact)
	if strings.Contains(got, "SSSSSSSS") {
		t.Fatalf("a secret value survived with its label cut off (%d bytes)", len(got))
	}
}

func TestRequestBodyDoesNotEscapeHTML(t *testing.T) {
	got := RequestBody(&HookEvent{Messages: []json.RawMessage{json.RawMessage(`{"role":"user","content":"a<b>&c"}`)}}, nil)
	if !strings.Contains(got, "a<b>&c") {
		t.Errorf("html escaped: %s", got)
	}
}

func TestRequestBodyOfAnInvalidMessageIsEmptyAndTraced(t *testing.T) {
	setHookEnv(t)
	if got := RequestBody(&HookEvent{SessionID: "s-bad", Messages: []json.RawMessage{json.RawMessage(`{bad`)}}, nil); got != "" {
		t.Errorf("body = %q", got)
	}
	var found bool
	for _, r := range traceRecords(t, trace.StageCapture) {
		found = found || (r.Outcome == "muse.request_body" && r.Detail["reason"] == "invalid_message")
	}
	if !found {
		t.Error("no finding traced")
	}
}
