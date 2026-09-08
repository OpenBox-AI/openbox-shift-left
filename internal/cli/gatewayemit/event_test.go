package gatewayemit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
)

const testDID = "did:aip:7f3c9b2e-0000-5000-a000-00000000beef"

func sampleCaptured() gateway.Captured {
	return gateway.Captured{
		HTTPMethod:            "POST",
		HTTPURL:               "https://api.anthropic.com/v1/messages",
		HTTPStatus:            200,
		CredentialFingerprint: "a1b2c3d4e5f60718",
		RequestHeaders:        map[string]string{"Authorization": "[redacted]", "Anthropic-Version": "2023-06-01"},
		ResponseHeaders:       map[string]string{"Request-Id": "req_upstream_1"},
		RequestBody:           `{"model":"claude-opus-4","messages":[]}`,
		ResponseBody:          `{"type":"message","role":"assistant"}`,
	}
}

func sampleIdentity() Identity {
	return Identity{SessionID: "sess-1", DeveloperDID: testDID}
}

var sampleAt = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// measuredCaptured is a relayed call the relay actually timed, which is every
// real one. sampleCaptured deliberately carries no timing: it is the
// unmeasured fallback path (a refusal, an unreachable upstream), and keeping it
// that way is what lets keypin_test.go pin the shipped idempotency key, since
// the key hashes the event timestamp.
func measuredCaptured() gateway.Captured {
	c := sampleCaptured()
	c.StartedAt = sampleAt
	c.EndedAt = sampleAt.Add(2200 * time.Millisecond)
	return c
}

// TestEventTypeIsTurnCompleted is not a taste assertion. Client/payload.go
// attaches a gateway span only under `case EventTurnCompleted`; every other
// event type drops Span on the floor with no error anywhere.
func TestEventTypeIsTurnCompleted(t *testing.T) {
	ev := mustEvent(LaneGateway, sampleIdentity(), "req-1", sampleAt, sampleCaptured())
	if ev.EventType != client.EventTurnCompleted {
		t.Fatalf("EventType = %q, want %q; no other type attaches the span", ev.EventType, client.EventTurnCompleted)
	}
}

// TestGatewayRequestIDIsSet keeps the two turn producers in disjoint activity-
// id namespaces (that decision requirement 8).
func TestGatewayRequestIDIsSet(t *testing.T) {
	ev := mustEvent(LaneGateway, sampleIdentity(), "req-1", sampleAt, sampleCaptured())
	if ev.GatewayRequestID != "req-1" {
		t.Errorf("GatewayRequestID = %q, want the relayed call's id", ev.GatewayRequestID)
	}
}

// TestEventIDIsDeterministicPerCall is INV-5. The spool can be drained by a
// different process long after the daemon that wrote it exited, and a retry
// must present the same idempotency key or core counts the call twice.
func TestEventIDIsDeterministicPerCall(t *testing.T) {
	a := mustEvent(LaneGateway, sampleIdentity(), "req-1", sampleAt, sampleCaptured())
	b := mustEvent(LaneGateway, sampleIdentity(), "req-1", sampleAt, sampleCaptured())
	if a.EventID == "" {
		t.Fatal("EventID empty; client.Emit rejects the event outright")
	}
	if a.EventID != b.EventID {
		t.Errorf("EventID not stable: %q vs %q", a.EventID, b.EventID)
	}
	c := mustEvent(LaneGateway, sampleIdentity(), "req-2", sampleAt, sampleCaptured())
	if a.EventID == c.EventID {
		t.Error("two distinct calls share one idempotency key; the second would be absorbed as a duplicate")
	}
}

// TestSessionAndDIDAreCarried; client.Emit rejects an empty SessionID, and the
// DID is what core groups the session under.
func TestSessionAndDIDAreCarried(t *testing.T) {
	ev := mustEvent(LaneGateway, sampleIdentity(), "req-1", sampleAt, sampleCaptured())
	if ev.SessionID != "sess-1" {
		t.Errorf("SessionID = %q", ev.SessionID)
	}
	if ev.DeveloperDID != testDID {
		t.Errorf("DeveloperDID = %q", ev.DeveloperDID)
	}
}

// TestObservedExchangeReachesTheWire is the assertion that counts.
func TestObservedExchangeReachesTheWire(t *testing.T) {
	pair := mustPair(LaneGateway, sampleIdentity(), "req-1", sampleAt, measuredCaptured())

	type payload struct {
		ActivityID     string          `json:"activity_id"`
		ActivityType   string          `json:"activity_type"`
		ActivityInput  json.RawMessage `json:"activity_input"`
		ActivityOutput json.RawMessage `json:"activity_output"`
		DurationMs     *float64        `json:"duration_ms"`
		Metadata       struct {
			CredentialFingerprint string `json:"credential_fingerprint"`
		} `json:"metadata"`
		SpanCount int   `json:"span_count"`
		Spans     []any `json:"spans"`
	}
	decode := func(ev client.DevEvent) payload {
		var p payload
		if err := json.Unmarshal(postThroughRealClient(t, ev, true), &p); err != nil {
			t.Fatalf("unmarshal posted payload: %v", err)
		}
		return p
	}
	started, completed := decode(pair[0]), decode(pair[1])

	// The bodies, in the fields that actually persist. They used to ride spans[],
	// which core parses on the normal path and then discards, so every one of
	// these assertions passed while nothing was stored.
	if !strings.Contains(string(started.ActivityInput), "claude-opus-4") {
		t.Errorf("the request body did not reach activity_input: %s", started.ActivityInput)
	}
	if !strings.Contains(string(completed.ActivityOutput), "assistant") {
		t.Errorf("the response body did not reach activity_output: %s", completed.ActivityOutput)
	}

	// And NOT under reply_text. Core judges an assistant turn on that key's
	// presence, one judge call per row, with no fallback to `content`. A relayed
	// model call is a model call, not a turn -- this lane carries ~137 rows to
	// the hook lane's 2 in a live session, so a reply_text here multiplies the
	// judge load by roughly 70 against a fleet doing ~7 judgements a minute.
	// Asserted on the posted bytes, in the lane that would pay the cost.
	if strings.Contains(string(completed.ActivityOutput), "reply_text") {
		t.Errorf("a relayed call carries reply_text, so core will judge every model call "+
			"instead of every turn: %s", completed.ActivityOutput)
	}

	// The measured call, which the relay used to compute and throw away.
	if completed.DurationMs == nil {
		t.Fatal("duration_ms is null on the completed half; the relay measured the call")
	}
	if want := 2200.0; *completed.DurationMs != want {
		t.Errorf("duration_ms = %v, want the relay's measured %v -- not the governance round trip, "+
			"which api_response_ms already reports and which is easily mistaken for this", *completed.DurationMs, want)
	}
	if started.DurationMs != nil {
		t.Errorf("the opening half carries duration_ms = %v, which claims the call was already over", *started.DurationMs)
	}

	for name, p := range map[string]payload{"started": started, "completed": completed} {
		if p.ActivityType != client.ActivityTypeLLMCompletion {
			t.Errorf("%s: activity_type = %q", name, p.ActivityType)
		}
		if !strings.Contains(p.ActivityID, ":gateway:") {
			t.Errorf("%s: activity_id %q is not in the gateway namespace; it could collide with a hook turn",
				name, p.ActivityID)
		}
		// Rehomed from the span's attributes, which never persisted for a developer
		// session -- so account binding has in fact never had anything to match on.
		if p.Metadata.CredentialFingerprint == "" {
			t.Errorf("%s: credential fingerprint absent from metadata; account binding has nothing to match on", name)
		}
		if p.SpanCount != 0 || len(p.Spans) != 0 {
			t.Errorf("%s: posted span_count=%d spans=%d, want none", name, p.SpanCount, len(p.Spans))
		}
	}
	if started.ActivityID != completed.ActivityID {
		t.Errorf("the pair split across activity_ids %q and %q", started.ActivityID, completed.ActivityID)
	}
}

// TestCaptureOffStripsBodiesButKeepsTheFingerprint is the other half of the
// gate, asserted on outbound bytes for the same reason, and on BOTH halves of
// the pair: a gate that held on one would leak on every model call.
//
// The headers are no longer part of this test's subject because they no longer
// egress at all. They only ever reached core inside spans[], which is discarded,
// and they were the highest-risk class this client carried -- the developer's
// live provider credential is on every model request.
func TestCaptureOffStripsBodiesButKeepsTheFingerprint(t *testing.T) {
	c := sampleCaptured()
	c.RequestHeaders = map[string]string{"Authorization": "[redacted]", "Anthropic-Version": "2023-06-01"}

	for _, ev := range mustPair(LaneGateway, sampleIdentity(), "req-1", sampleAt, c) {
		got := string(postThroughRealClient(t, ev, false))

		if strings.Contains(got, "claude-opus-4") || strings.Contains(got, `"role":"assistant"`) {
			t.Errorf("%s: bodies egressed with content capture OFF", ev.EventType)
		}
		if strings.Contains(got, "Anthropic-Version") {
			t.Errorf("%s: headers egressed", ev.EventType)
		}
		if !strings.Contains(got, "a1b2c3d4e5f60718") {
			t.Errorf("%s: credential fingerprint disappeared under the content gate; that decision keeps it ungated", ev.EventType)
		}
	}
}

func postThroughRealClient(t *testing.T, ev client.DevEvent, contentOn bool) []byte {
	t.Helper()

	var captured []byte
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"decision":"ALLOW"}`)
	}))
	defer srv.Close()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	cl, err := client.New(client.Config{
		BaseURL:               srv.URL,
		APIKey:                "obx_test",
		DID:                   testDID,
		PrivateKeyB64:         base64.StdEncoding.EncodeToString(priv.Seed()),
		ContentCaptureEnabled: contentOn,
	})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	if _, err := cl.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(captured) == 0 {
		t.Fatal("nothing was POSTed")
	}
	return captured
}
