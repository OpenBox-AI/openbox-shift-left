package gatewayemit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/conformance"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
)

const (
	chatConvA = "0f8e2d4c-6b1a-4c3e-9d7f-2a5b8c1e4f60"
	chatConvB = "7a3c9e1f-2d4b-4f6a-8c0e-5b7d9f1a3c5e"
	chatOrg   = "5c1d9a7e-3b2f-4e8a-b6c4-9f0e1d2a3b4c"

	desktopUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.0.211 Chrome/134.0.6998.205 Electron/35.2.1 Safari/537.36"
	chromeUA  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
)

func chatCaptured(conv, userAgent string) gateway.Captured {
	return gateway.Captured{
		HTTPMethod: "POST",
		HTTPURL:    "https://claude.ai/api/organizations/" + chatOrg + "/chat_conversations/" + conv + "/completion",
		HTTPStatus: 200,
		RequestHeaders: map[string]string{
			"Cookie":     "[redacted]",
			"User-Agent": userAgent,
		},
		ResponseHeaders: map[string]string{"Request-Id": "req_chat_" + conv[:8]},
		RequestBody:     `{"prompt":"hello","timezone":"UTC"}`,
		ResponseBody:    "event: completion\ndata: {\"completion\":\"hi\"}\n\n",
	}
}

func newChatEmitter(t *testing.T) (*Emitter, *fakeDelivery) {
	t.Helper()
	em, delivery, _ := newTestEmitter(t)
	em.Lane = LaneProxy
	return em, delivery
}

// TestChatCompletionOpensOneSessionPerConversation the first completion of a
// conversation is announced with a minted SessionStarted, because no hook will
// ever open it; every completion then files its own Started/Completed pair
// under the conversation's key.
func TestChatCompletionOpensOneSessionPerConversation(t *testing.T) {
	em, delivery := newChatEmitter(t)
	em.Emit(context.Background(), chatCaptured(chatConvA, desktopUA))
	em.Emit(context.Background(), chatCaptured(chatConvA, desktopUA))

	key := "chat:claude-ai:" + chatConvA
	evs := delivery.forSession(key)
	var types []client.EventType
	for _, ev := range evs {
		types = append(types, ev.EventType)
	}
	want := []client.EventType{client.EventSessionStarted,
		client.EventTurnStarted, client.EventTurnCompleted,
		client.EventTurnStarted, client.EventTurnCompleted}
	if len(types) != len(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events = %v, want %v", types, want)
		}
	}
	for _, ev := range evs {
		if ev.Metadata["surface"] != "claude-ai" || ev.Metadata["chat_client"] != "desktop" {
			t.Errorf("%s metadata = %v, want surface=claude-ai chat_client=desktop", ev.EventType, ev.Metadata)
		}
		if ev.Tool.Name != chatToolName {
			t.Errorf("%s tool = %q, want %q", ev.EventType, ev.Tool.Name, chatToolName)
		}
		if ev.DeveloperDID != testDID {
			t.Errorf("%s signed as %q, want the claude-code identity %q", ev.EventType, ev.DeveloperDID, testDID)
		}
	}
	if evs[1].Span.RequestBody == "" || evs[2].Span.ResponseBody == "" {
		t.Error("a chat completion carries its request and response bodies")
	}
	if evs[1].ActivityType != client.ActivityTypeLLMCompletion {
		t.Errorf("activity type = %q, want llm_completion", evs[1].ActivityType)
	}
}

// TestChatConversationsNeverMerge two conversations are two sessions, and a
// browser chat is labelled as one.
func TestChatConversationsNeverMerge(t *testing.T) {
	em, delivery := newChatEmitter(t)
	em.Emit(context.Background(), chatCaptured(chatConvA, desktopUA))
	em.Emit(context.Background(), chatCaptured(chatConvB, chromeUA))

	a := delivery.forSession("chat:claude-ai:" + chatConvA)
	b := delivery.forSession("chat:claude-ai:" + chatConvB)
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("got %d and %d events, want 3 each (SessionStarted + pair)", len(a), len(b))
	}
	if b[0].Metadata["chat_client"] != "browser" {
		t.Errorf("chat_client = %v, want browser for a Chrome user agent", b[0].Metadata["chat_client"])
	}
}

// TestChatSessionStartedIsIdempotentAcrossDaemonRestarts a restarted daemon
// forgets which conversations it announced and announces them again; the
// idempotency key must be the same both times, so core keeps one.
func TestChatSessionStartedIsIdempotentAcrossDaemonRestarts(t *testing.T) {
	first, d1 := newChatEmitter(t)
	first.Emit(context.Background(), chatCaptured(chatConvA, desktopUA))
	second, d2 := newChatEmitter(t)
	second.Emit(context.Background(), chatCaptured(chatConvA, chromeUA))

	key := "chat:claude-ai:" + chatConvA
	s1, s2 := d1.forSession(key)[0], d2.forSession(key)[0]
	if s1.EventType != client.EventSessionStarted || s2.EventType != client.EventSessionStarted {
		t.Fatalf("first events = %s, %s; want SessionStarted", s1.EventType, s2.EventType)
	}
	if s1.EventID == "" || s1.EventID != s2.EventID {
		t.Fatalf("SessionStarted event ids differ across restarts: %q vs %q", s1.EventID, s2.EventID)
	}
}

// TestChatIsRecordedWhateverLaneIsElected the election decides which lane
// describes a Code session's model calls; a chat has exactly one possible
// producer, this relay, so losing the election must not silence it.
func TestChatIsRecordedWhateverLaneIsElected(t *testing.T) {
	em, delivery := newChatEmitter(t)
	em.Elected = func() bool { return false }
	em.ElectedName = func() string { return "telemetry" }

	em.Emit(context.Background(), chatCaptured(chatConvA, desktopUA))
	if got := len(delivery.forSession("chat:claude-ai:" + chatConvA)); got != 3 {
		t.Fatalf("got %d chat events with another lane elected, want 3", got)
	}

	em.Emit(context.Background(), capturedWithSession("code-session"))
	if got := len(delivery.forSession("code-session")); got != 0 {
		t.Fatalf("a Code call was recorded by a lane that lost the election (%d events)", got)
	}
}

// TestChatIsOnlyTheRelaysToProduce the gateway lane never sees claude.ai, and
// an emitter for it that somehow did must not mint a chat key.
func TestChatIsOnlyTheRelaysToProduce(t *testing.T) {
	em, delivery, _ := newTestEmitter(t)
	em.Emit(context.Background(), chatCaptured(chatConvA, desktopUA))
	if got := len(delivery.forSession("chat:claude-ai:" + chatConvA)); got != 0 {
		t.Fatalf("the gateway lane produced %d chat events", got)
	}
}

// TestChatSessionStartedRefusedStopsThePair a saturated pool refusing the
// announcement must not leave activity rows under a session core never saw
// opened; the next completion retries the announcement.
func TestChatSessionStartedRefusedStopsThePair(t *testing.T) {
	em, delivery := newChatEmitter(t)
	delivery.Accept = false
	em.Emit(context.Background(), chatCaptured(chatConvA, desktopUA))
	delivery.Accept = true
	em.Emit(context.Background(), chatCaptured(chatConvA, desktopUA))

	evs := delivery.forSession("chat:claude-ai:" + chatConvA)
	if len(evs) != 3 || evs[0].EventType != client.EventSessionStarted {
		t.Fatalf("after a refused announcement the next completion must re-announce; got %d events", len(evs))
	}
}

// TestNonCompletionClaudeAICallsSendNothing the chat app's title, notices and
// conversation reads are not model calls and carry no conversation key.
func TestNonCompletionClaudeAICallsSendNothing(t *testing.T) {
	em, delivery := newChatEmitter(t)
	c := chatCaptured(chatConvA, desktopUA)
	c.HTTPURL = strings.Replace(c.HTTPURL, "/completion", "/title", 1)
	em.Emit(context.Background(), c)
	if n := len(delivery.events); n != 0 {
		t.Fatalf("a claude.ai title call produced events for %d session(s)", n)
	}
}

// TestChatEventsSatisfyTheDevEventContract a minted session and its pair are
// ordinary dev events on the wire contract: same schema, same content rules.
func TestChatEventsSatisfyTheDevEventContract(t *testing.T) {
	em, delivery := newChatEmitter(t)
	em.Emit(context.Background(), chatCaptured(chatConvA, chromeUA))
	evs := delivery.forSession("chat:claude-ai:" + chatConvA)
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3", len(evs))
	}
	for _, ev := range evs {
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if err := conformance.ValidateDevEvent(raw, true); err != nil {
			t.Errorf("%s violates the dev-event contract: %v", ev.EventType, err)
		}
	}
}
