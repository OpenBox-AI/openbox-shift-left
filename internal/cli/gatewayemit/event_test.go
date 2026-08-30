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
	body := postThroughRealClient(t, mustEvent(LaneGateway, sampleIdentity(), "req-1", sampleAt, sampleCaptured()), true)

	var p struct {
		ActivityID string `json:"activity_id"`
		SpanCount  int    `json:"span_count"`
		Spans      []struct {
			SpanID                string            `json:"span_id"`
			HTTPMethod            string            `json:"http_method"`
			HTTPURL               string            `json:"http_url"`
			HTTPStatus            int               `json:"http_status_code"`
			CredentialFingerprint string            `json:"credential_fingerprint"`
			RequestHeaders        map[string]string `json:"request_headers"`
			ResponseHeaders       map[string]string `json:"response_headers"`
			RequestBody           string            `json:"request_body"`
			ResponseBody          string            `json:"response_body"`
			Attributes            map[string]any    `json:"attributes"`
		} `json:"spans"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("unmarshal posted payload: %v", err)
	}
	if p.SpanCount != 1 || len(p.Spans) != 1 {
		t.Fatalf("span_count=%d spans=%d, want exactly one; the capture never reached the wire", p.SpanCount, len(p.Spans))
	}
	s := p.Spans[0]
	if s.HTTPMethod != "POST" || s.HTTPStatus != 200 {
		t.Errorf("classification fields wrong: method=%q status=%d (core recomputes semantic_type from these)", s.HTTPMethod, s.HTTPStatus)
	}
	if !strings.Contains(s.HTTPURL, "api.anthropic.com") {
		t.Errorf("http_url = %q; isLLMCall needs an LLM domain here or the span classifies as something else", s.HTTPURL)
	}
	if s.CredentialFingerprint == "" || s.Attributes["openbox.credential_fingerprint"] == nil {
		t.Error("credential fingerprint absent; account binding has nothing to match on")
	}
	if s.RequestHeaders["Anthropic-Version"] != "2023-06-01" {
		t.Errorf("request headers did not reach the wire: %v", s.RequestHeaders)
	}
	if s.ResponseHeaders["Request-Id"] != "req_upstream_1" {
		t.Errorf("response headers did not reach the wire: %v", s.ResponseHeaders)
	}
	if !strings.Contains(s.RequestBody, "claude-opus-4") || !strings.Contains(s.ResponseBody, "assistant") {
		t.Errorf("bodies did not reach the wire: req=%q resp=%q", s.RequestBody, s.ResponseBody)
	}
	if !strings.Contains(p.ActivityID, ":gateway:") {
		t.Errorf("activity_id %q is not in the gateway namespace; it could collide with a hook turn", p.ActivityID)
	}
}

// TestCaptureOffStripsBodiesAndHeadersButKeepsTheFingerprint is the other half
// of the gate, asserted on outbound bytes for the same reason.
func TestCaptureOffStripsBodiesAndHeadersButKeepsTheFingerprint(t *testing.T) {
	c := sampleCaptured()
	c.RequestHeaders = map[string]string{"Authorization": "[redacted]", "Anthropic-Version": "2023-06-01"}
	raw := postThroughRealClient(t, mustEvent(LaneGateway, sampleIdentity(), "req-1", sampleAt, c), false)
	got := string(raw)

	if strings.Contains(got, "claude-opus-4") || strings.Contains(got, `"role":"assistant"`) {
		t.Error("bodies egressed with content capture OFF")
	}
	if strings.Contains(got, "Anthropic-Version") {
		t.Error("headers egressed with content capture OFF")
	}
	if !strings.Contains(got, "a1b2c3d4e5f60718") {
		t.Error("credential fingerprint disappeared under the content gate; that decision keeps it ungated")
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
