package sessionkey

import "testing"

const chatConv = "0f8e2d4c-6b1a-4c3e-9d7f-2a5b8c1e4f60"

func chatPath(conv string) string {
	return "/api/organizations/5c1d9a7e-3b2f-4e8a-b6c4-9f0e1d2a3b4c/chat_conversations/" + conv + "/completion"
}

func TestResolveChatKeysAClaudeAICompletionOnItsConversation(t *testing.T) {
	for _, host := range []string{"claude.ai", "CLAUDE.AI", "www.claude.ai", "claude.ai:443"} {
		key, ok := ResolveChat(host, chatPath(chatConv))
		if !ok || key != "chat:claude-ai:"+chatConv {
			t.Errorf("ResolveChat(%q) = (%q, %v), want chat:claude-ai:%s", host, key, ok, chatConv)
		}
	}
}

// TestResolveChatNeverKeysAnAPIHost the chat namespace is a different key
// space only a chat surface can produce: an API host carrying the same path
// shape must never mint one, or the synthetic-session ban reopens.
func TestResolveChatNeverKeysAnAPIHost(t *testing.T) {
	for _, host := range []string{"api.anthropic.com", "notclaude.ai", "claude.ai.example.com", "127.0.0.1:8790", ""} {
		if key, ok := ResolveChat(host, chatPath(chatConv)); ok {
			t.Errorf("ResolveChat(%q) minted %q; only a claude.ai host may", host, key)
		}
	}
}

// TestResolveChatRefusesAnythingButAProvedCompletionPath only the completion
// path is proved on the wire; the title, notices and conversation reads on the
// same host are not model calls, and a garbled or non-canonical id is not a
// key worth keeping.
func TestResolveChatRefusesAnythingButAProvedCompletionPath(t *testing.T) {
	org := "/api/organizations/5c1d9a7e-3b2f-4e8a-b6c4-9f0e1d2a3b4c/chat_conversations/"
	for _, path := range []string{
		org + chatConv + "/title",
		org + chatConv + "/composer_notices",
		org + chatConv,
		org + "0F8E2D4C-6B1A-4C3E-9D7F-2A5B8C1E4F60/completion",
		org + "not-a-uuid/completion",
		org + chatConv + "/completion/extra",
		"/v1/messages",
	} {
		if key, ok := ResolveChat("claude.ai", path); ok {
			t.Errorf("ResolveChat(claude.ai, %q) = %q; want no key", path, key)
		}
	}
}
