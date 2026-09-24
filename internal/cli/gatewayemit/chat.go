package gatewayemit

import (
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/sessionkey"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
)

// chatToolName is Tool.Name on every chat record: the surface a person typed
// into, not Claude Code, which never saw the conversation.
const chatToolName = "claude-ai"

// maxAnnouncedChats bounds the set of conversations this daemon has opened a
// session for. Past it the set is dropped and conversations are announced
// again, which costs a duplicate core discards: the announcement's
// idempotency key depends on the conversation alone.
const maxAnnouncedChats = 4096

// chatSessionEventRequest is the fixed request-id part of a chat
// announcement's idempotency key, so the key names the conversation only.
const chatSessionEventRequest = "chat-session"

// chatClient reports which claude.ai client made the call, from its
// User-Agent: the desktop app is Electron and names itself Claude/<ver>.
// Metadata only; the conversation, not the client, is the session.
func chatClient(headers map[string]string) string {
	ua := headers["User-Agent"]
	if strings.Contains(ua, "Electron/") || strings.Contains(ua, " Claude/") {
		return "desktop"
	}
	return "browser"
}

func chatMetadata(c gateway.Captured) map[string]any {
	return map[string]any{
		"surface":     sessionkey.ChatSurfaceClaudeAI,
		"chat_client": chatClient(c.RequestHeaders),
	}
}

// chatSessionStarted is the minted SessionStarted a chat session opens with.
// No hook will ever open a chat, and a chat gets no SessionEnded either: the
// relay sees no end to a conversation.
func (e *Emitter) chatSessionStarted(id Identity, at time.Time, meta map[string]any) client.DevEvent {
	ev := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventType:     client.EventSessionStarted,
		SessionID:     id.SessionID,
		DeveloperDID:  id.DeveloperDID,
		Tool:          client.Tool{Name: chatToolName, Kind: client.ToolShell},
		Timestamp:     at.UTC().Format(time.RFC3339Nano),
		Metadata:      meta,
	}
	ev.EventID = eventID(e.Lane.IDPrefix, id.SessionID, chatSessionEventRequest, string(ev.EventType))
	return ev
}

// chatAnnounced reports whether this daemon already opened key's session.
func (e *Emitter) chatAnnounced(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.announcedChats[key]
	return ok
}

func (e *Emitter) markChatAnnounced(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.announcedChats == nil || len(e.announcedChats) >= maxAnnouncedChats {
		e.announcedChats = make(map[string]struct{})
	}
	e.announcedChats[key] = struct{}{}
}
