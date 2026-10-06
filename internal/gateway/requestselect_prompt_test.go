package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// claudeAIShapedRequest reproduces a claude.ai chat completion body: the new turn
// as a top-level `prompt` near the HEAD, then a tool list that dwarfs it, then
// the surface's own bookkeeping keys. No `messages` array -- the history lives
// server side. The live rows that motivated the prompt path were exactly this
// layout, and a tail window over it held tool schemas and nothing else.
func claudeAIShapedRequest(prompt string, toolCount int) string {
	tools := strings.Repeat(`{"name":"issue_read","description":"Get information about a specific issue in a GitHub repository.","input_schema":{"type":"object"},"integration_name":"Github MCP"},`, toolCount)
	tools = "[" + strings.TrimSuffix(tools, ",") + "]"
	promptJSON, _ := json.Marshal(prompt)
	return fmt.Sprintf(`{"prompt":%s,"parent_message_uuid":"00000000-0000-0000-0000-000000000001","timezone":"Asia/Saigon","locale":"en-US","tools":%s,"attachments":[],"files":[],"rendering_mode":"messages","create_conversation_params":{"name":"","model":"claude-opus-5-5"}}`,
		promptJSON, tools)
}

// TestAClaudeAIPromptSurvivesSelection is the load-bearing assertion, and it
// fails on the tree before the prompt path: with no `messages` key the selector
// fell back to a tail window, which on this layout is all tools.
func TestAClaudeAIPromptSurvivesSelection(t *testing.T) {
	const prompt = "what is the capital of Vietnam?"
	body := claudeAIShapedRequest(prompt, 600)
	if len(body) <= selectionBudget {
		t.Fatalf("fixture is %d bytes; it must exceed the %d budget to exercise selection", len(body), selectionBudget)
	}

	got := selectModelCallRequest(body)

	if strings.HasPrefix(got, markerPrefix) {
		t.Fatalf("selection fell back to a window: %.200q", got)
	}
	var doc struct {
		Prompt    string         `json:"prompt"`
		Selection *selectionNote `json:"openbox_selection"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("selected document does not parse: %v", err)
	}
	if doc.Prompt != prompt {
		t.Errorf("prompt = %q, want %q", doc.Prompt, prompt)
	}
	if doc.Selection == nil || !contains(doc.Selection.DroppedKeys, "tools") {
		t.Errorf("the note must report `tools` as dropped: %+v", doc.Selection)
	}
	if doc.Selection != nil && doc.Selection.OriginalBytes != len(body) {
		t.Errorf("original_bytes = %d, want %d", doc.Selection.OriginalBytes, len(body))
	}
	if strings.Contains(got, "issue_read") {
		t.Error("tool definitions reached the selected document")
	}
	if !strings.HasSuffix(got, `"prompt":"`+prompt+`"}`) {
		t.Errorf("prompt must be the last key, so the judge's tail keeps it: %.200q", got)
	}
}

// TestAnOverBudgetPromptKeepsItsMarkedTail: a pasted document can outgrow the
// budget on its own, and its newest bytes are the ones worth keeping.
func TestAnOverBudgetPromptKeepsItsMarkedTail(t *testing.T) {
	prompt := strings.Repeat("a", 2*selectionBudget) + "THE-END"
	got := selectModelCallRequest(claudeAIShapedRequest(prompt, 10))

	if len(got) > selectionBudget {
		t.Fatalf("selected document is %d bytes, over the %d budget", len(got), selectionBudget)
	}
	var doc struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("selected document does not parse: %v", err)
	}
	if !strings.HasPrefix(doc.Prompt, markerPrefix) || !strings.HasSuffix(doc.Prompt, "THE-END") {
		t.Errorf("prompt must be a marked tail ending in the newest bytes; got %.80q...%.40q",
			doc.Prompt, doc.Prompt[max(0, len(doc.Prompt)-40):])
	}
}

// TestANonStringPromptFallsBackWithAMarker: the bind is one key name, never a
// schema, so an unexpected type degrades to the marked window.
func TestANonStringPromptFallsBackWithAMarker(t *testing.T) {
	got := selectModelCallRequest(`{"prompt":{"text":"hi"},"tools":[]}`)
	if !strings.Contains(got, "prompt is not a string") {
		t.Errorf("want a marker naming the prompt, got %.200q", got)
	}
}

// TestAPromptDocumentSurvivesTheWholeCapturePath runs redaction and the
// recovery re-selection too: the prompt path's output must be one the second
// pass binds again rather than windows.
func TestAPromptDocumentSurvivesTheWholeCapturePath(t *testing.T) {
	const prompt = "summarise the design doc"
	got := captureRequestBody(selectModelCallRequest(claudeAIShapedRequest(prompt, 600)))
	var doc struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil || doc.Prompt != prompt {
		t.Errorf("stored request = %.200q, want a document whose prompt is %q", got, prompt)
	}
}

// TestASmallPromptOnlyBodyCarriesNoNote: nothing dropped means no note, the
// same rule the messages path follows.
func TestASmallPromptOnlyBodyCarriesNoNote(t *testing.T) {
	if got := selectModelCallRequest(`{"prompt":"hi"}`); got != `{"prompt":"hi"}` {
		t.Errorf("got %q, want the prompt alone with no note", got)
	}
}

// TestACompressedRequestLargerThanTheResponseBoundIsSelectedWhole is the
// Claude Desktop case, wired through the relay: its claude.ai request arrives
// content-encoded and decodes past 256 KiB. Bounded by the response path's head
// cut, the decoded JSON did not parse and the stored row was a tail window of
// tool schemas; decoded whole, the selector finds the prompt.
func TestACompressedRequestLargerThanTheResponseBoundIsSelectedWhole(t *testing.T) {
	const prompt = "draft the release notes"
	plain := claudeAIShapedRequest(prompt, 3000)
	if len(plain) <= maxCaptureInputBytes {
		t.Fatalf("fixture is %d bytes; it must exceed the %d response bound", len(plain), maxCaptureInputBytes)
	}

	var got recorded
	upstream := upstreamRecorder(t, &got, nil)
	em := &recordingEmitter{}
	srv := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/append_message", bytes.NewReader(gzipOf(t, plain)))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/json")
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()

	captured := em.await(t, 1)[0].RequestBody
	var doc struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(captured), &doc); err != nil || doc.Prompt != prompt {
		t.Errorf("captured request = %.200q, want a document whose prompt is %q", captured, prompt)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
