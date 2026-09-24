package main

import (
	"net/http/httptest"
	"testing"
)

// TestRelayKeepsAChatCompletionBody the relay decides per request whether a
// body is kept, before any capture exists; a claude.ai chat completion is a
// model call and keeps its body, while the same path on api.anthropic.com is
// Claude Code's own telemetry and keeps none.
func TestRelayKeepsAChatCompletionBody(t *testing.T) {
	const path = "/api/organizations/5c1d9a7e-3b2f-4e8a-b6c4-9f0e1d2a3b4c/chat_conversations/" +
		"0f8e2d4c-6b1a-4c3e-9d7f-2a5b8c1e4f60/completion"
	chat := httptest.NewRequest("POST", "https://claude.ai"+path, nil)
	if !relayCapturesBody(chat) {
		t.Error("a claude.ai chat completion must keep its body")
	}
	telemetry := httptest.NewRequest("POST", "https://api.anthropic.com"+path, nil)
	if relayCapturesBody(telemetry) {
		t.Error("the same path on api.anthropic.com is tool telemetry and keeps no body")
	}
	messages := httptest.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	if !relayCapturesBody(messages) {
		t.Error("a Claude Code completion must still keep its body")
	}
}
