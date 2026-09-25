package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayemit"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/sessionkey"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
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

const (
	testChatOrg  = "5c1d9a7e-3b2f-4e8a-b6c4-9f0e1d2a3b4c"
	testChatConv = "0f8e2d4c-6b1a-4c3e-9d7f-2a5b8c1e4f60"
)

// chatCapturedCall builds a claude.ai chat completion Captured -- the same
// shape the real relay's own emitter.Emit sees -- for one conversation, so
// chat routing tests can exercise gatewayemit.Emitter/routeLaneRecord/
// newChatPool directly, at the seam these tests actually own, rather than
// through a real network dial (a real proxy chain requires an empty
// Config.Upstream so the captured HTTPURL's own host is genuinely claude.ai,
// which only resolves through a real dial to it -- exactly the sandbox
// egress dependency every other test in this package avoids).
func chatCapturedCall(conv string) gateway.Captured {
	return gateway.Captured{
		HTTPMethod: "POST",
		HTTPURL:    "https://claude.ai/api/organizations/" + testChatOrg + "/chat_conversations/" + conv + "/completion",
		HTTPStatus: 200,
		RequestHeaders: map[string]string{
			"User-Agent": "Mozilla/5.0 Chrome/140.0.0.0 Safari/537.36",
		},
		ResponseHeaders: map[string]string{"Request-Id": "req_chat_" + conv[:8]},
		RequestBody:     `{"prompt":"hello"}`,
		ResponseBody:    "event: completion\ndata: {\"completion\":\"hi\"}\n\n",
	}
}

func testChatKey(conv string) string {
	key, ok := sessionkey.ResolveChat("claude.ai", "/api/organizations/"+testChatOrg+"/chat_conversations/"+conv+"/completion")
	if !ok {
		panic("testChatKey: ResolveChat refused its own fixture path")
	}
	return key
}

func waitUntilChatHalted(t *testing.T, key string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, halted := hookflow.SessionHalted(key); halted {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("conversation %s was never latched within %s", key, timeout)
}

// TestChatEventCoreDoesNotAcceptLatchesTheConversation pins: a chat event
// core answers with a 503 gets one attempt and exactly one retry (fakecore's
// own AttemptsByKey), the
// conversation is latched (HaltOnDeliveryFailure, through newChatPool's own
// deliver closure), and the NEXT completion of that SAME conversation is
// then refused by haltDecorator naming the event -- while a DIFFERENT
// conversation is untouched.
func TestChatEventCoreDoesNotAcceptLatchesTheConversation(t *testing.T) {
	memhttptest.RequireBind(t)

	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	t.Setenv(devconfig.EnvHome, t.TempDir())
	t.Setenv(devconfig.EnvEnforcementFile, filepath.Join(t.TempDir(), "enforcements.jsonl"))

	fake := fakecore.New(t, fakecore.Script{AlwaysStatus: 503})
	t.Setenv(devconfig.EnvBaseURL, fake.URL())
	seedV3EnvIdentity(t)

	logger := log.New(io.Discard, "", 0)
	identities := resolveProviderIdentities(logger.Printf)
	_, chatDeliver := newChatPool(identities, &hookflow.Advisory{}, logger)

	em := &gatewayemit.Emitter{
		Lane:    gatewayemit.LaneProxy,
		Elected: func() bool { return true },
		Deliver: routeLaneRecord(nil, chatDeliver, logger, "transport"),
		DID:     devconfig.ResolveDIDOrEmpty,
		Warn:    logger.Printf,
	}

	em.Emit(context.Background(), chatCapturedCall(testChatConv))

	key := testChatKey(testChatConv)
	waitUntilChatHalted(t, key, 5*time.Second)

	for k, n := range fake.AttemptsByKey() {
		if n != 2 {
			t.Errorf("event (idempotency key %s) attempted %d time(s), want exactly 2 (one attempt, one retry)", k, n)
		}
	}

	// The run's next completion for the SAME conversation is refused.
	eval, err := (haltDecorator{logger: logger}).Evaluate(context.Background(), chatCapturedCall(testChatConv))
	if err != nil {
		t.Fatalf("Evaluate (latched conversation): %v", err)
	}
	if eval.Verdict != client.VerdictHalt {
		t.Fatalf("a completion for the latched conversation resolved verdict %q, want HALT", eval.Verdict)
	}
	if !strings.Contains(eval.Reason, "could not record") {
		t.Errorf("refusal reason does not name the delivery failure: %q", eval.Reason)
	}

	// A DIFFERENT conversation is untouched.
	otherConv := "7a3c9e1f-2d4b-4f6a-8c0e-5b7d9f1a3c5e"
	eval, err = (haltDecorator{logger: logger}).Evaluate(context.Background(), chatCapturedCall(otherConv))
	if err != nil {
		t.Fatalf("Evaluate (unrelated conversation): %v", err)
	}
	if eval.Verdict != client.VerdictAllow {
		t.Fatalf("an unrelated conversation resolved verdict %q, want ALLOW", eval.Verdict)
	}
}

// TestChatPoolSaturationDropLatchesTheConversation pins: a chat event the
// pool could not even ACCEPT (saturated) never reached core either, so it
// latches the conversation exactly like an attempted-and-refused one.
func TestChatPoolSaturationDropLatchesTheConversation(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	release := make(chan struct{})
	defer close(release)
	var once sync.Once
	started := make(chan struct{})
	pool := hookflow.NewDeliverPool(1, time.Minute, func(ctx context.Context, ev client.DevEvent) {
		once.Do(func() { close(started) })
		<-release
	})
	logger := log.New(io.Discard, "", 0)
	deliver := func(ev client.DevEvent) bool {
		if pool.Submit(ev) {
			return true
		}
		hookflow.HaltOnDeliveryFailure(logger, ev, errChatPoolUnavailable)
		return false
	}

	// Saturate the pool's one slot with a first event that never returns
	// until release closes.
	first := client.DevEvent{EventID: "chat-e1", EventType: client.EventTurnStarted, SessionID: testChatKey(testChatConv)}
	if !deliver(first) {
		t.Fatal("the first event must be accepted into an empty pool")
	}
	<-started

	// A second event for the SAME conversation now finds the pool saturated.
	second := client.DevEvent{EventID: "chat-e2", EventType: client.EventTurnCompleted, SessionID: testChatKey(testChatConv)}
	if deliver(second) {
		t.Fatal("a saturated pool must refuse the second Submit")
	}

	if _, halted := hookflow.SessionHalted(testChatKey(testChatConv)); !halted {
		t.Fatal("a chat event the pool could not accept must still latch the conversation")
	}
}

// TestChatLatchFilenameHasNoColon pins: the latch filename for a
// chat:claude-ai:<uuid> key contains no ':' -- Windows-legal -- through the
// EXISTING haltPath sanitize+hash mechanism, not a chat-specific one.
func TestChatLatchFilenameHasNoColon(t *testing.T) {
	haltDir := t.TempDir()
	t.Setenv(devconfig.EnvHaltDir, haltDir)

	key := testChatKey(testChatConv)
	if !strings.Contains(key, ":") {
		t.Fatalf("fixture key %q does not even contain a ':' to prove sanitized away", key)
	}
	hookflow.HaltOnDeliveryFailure(log.New(io.Discard, "", 0), client.DevEvent{
		EventID: "e1", EventType: client.EventTurnStarted, SessionID: key,
	}, errors.New("503 service unavailable"))

	entries, err := os.ReadDir(haltDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("halt dir has %d entr(ies), want 1", len(entries))
	}
	if strings.Contains(entries[0].Name(), ":") {
		t.Errorf("latch filename %q contains ':', not Windows-legal", entries[0].Name())
	}
}
