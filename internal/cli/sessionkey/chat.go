package sessionkey

import (
	"net"
	"regexp"
	"strings"
)

// ChatSurfaceClaudeAI names the claude.ai chat surface (the web app and the
// Claude desktop app, which drive the same API) inside a chat key.
const ChatSurfaceClaudeAI = "claude-ai"

// chatKeyPrefix opens the chat key space. It is disjoint from every tool
// session id by construction: only ResolveChat produces it, and only for a
// chat host.
const chatKeyPrefix = "chat:"

// claudeAICompletionPath is the one claude.ai model-call path proved on the
// wire: the conversation id rides in the path, canonical lowercase. The org
// segment is matched loosely because it is not part of the key.
var claudeAICompletionPath = regexp.MustCompile(
	`^/api/organizations/[^/]+/chat_conversations/` +
		`([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/completion$`)

// ResolveChat returns the session key for a relayed claude.ai chat
// completion, "chat:claude-ai:<conversation-uuid>", keyed on the
// conversation so the same chat continued in the browser and the desktop app
// stays one session. It is the only producer of a chat key, and it refuses
// every host but claude.ai and its subdomains: an API host carrying the same
// path shape must never open a synthetic session.
func ResolveChat(host, path string) (string, bool) {
	if !IsClaudeAIHost(host) {
		return "", false
	}
	m := claudeAICompletionPath.FindStringSubmatch(path)
	if m == nil {
		return "", false
	}
	return chatKeyPrefix + ChatSurfaceClaudeAI + ":" + m[1], true
}

// IsChatKey reports whether key is in the chat key space.
func IsChatKey(key string) bool { return strings.HasPrefix(key, chatKeyPrefix) }

// IsClaudeAIHost reports whether host (with or without a port) is claude.ai
// or one of its subdomains, the same reach as the relay's claude.ai
// intercept row. ASCII case folding only, as the host table does.
func IsClaudeAIHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = asciiLower(strings.TrimSuffix(host, "."))
	return host == "claude.ai" || strings.HasSuffix(host, ".claude.ai")
}

// asciiLower folds A-Z only: a Unicode fold would make a lookalike host
// compare equal to the real one.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
