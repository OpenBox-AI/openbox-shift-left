package client

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testAPIKey = "obx_test_" + "0123456789abcdef0123456789abcdef0123456789abcdef"

// captureLogger records log lines so tests can assert fail-open diagnostics and,
// critically, that no secret ever appears in them (INV-1).
type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *captureLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *captureLogger) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func newTestClient(t *testing.T, baseURL string, contentOn bool) (*Client, *captureLogger) {
	t.Helper()
	log := &captureLogger{}
	c, err := New(Config{
		BaseURL:               baseURL,
		APIKey:                testAPIKey,
		DID:                   testDID,
		SeedB64:               testSeedB64,
		ContentCaptureEnabled: contentOn,
		RetryBase:             time.Millisecond, // keep retry tests fast
		Logger:                log,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, log
}

func sampleEvent() DevEvent {
	return DevEvent{
		SchemaVersion: SchemaVersion,
		EventID:       "evt-1",
		EventType:     EventToolCall,
		SessionID:     "sess-1",
		DeveloperDID:  testDID,
		Timestamp:     "2026-07-08T12:00:00Z",
		Tool:          Tool{Name: "Edit", Kind: ToolFile},
		Span:          &Span{SemanticType: "file_write", Stage: "started", FilePath: "/x.go"},
	}
}

// coreMirrorServer stands in for openbox-core's /evaluate: it authenticates the
// Bearer key, verifies the AIP signature exactly as core would, records the
// decoded payload, and returns a verdict. onRequest lets a test override the
// status/verdict per call (for retry tests).
func coreMirrorServer(t *testing.T, pub ed25519.PublicKey, onRequest func(n int) (status int, verdict string)) (*httptest.Server, *[]governanceEventPayload, *int) {
	t.Helper()
	var mu sync.Mutex
	var payloads []governanceEventPayload
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()

		if r.URL.Path != evaluatePath {
			t.Errorf("path = %q, want %q", r.URL.Path, evaluatePath)
		}
		if got := r.Header.Get(headerAuthorization); got != "Bearer "+testAPIKey {
			t.Errorf("Authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if err := verifyLikeCore(pub, r.Method, r.URL.Path, body, r.Header); err != nil {
			t.Errorf("core-mirror rejected signature: %v", err)
			w.WriteHeader(401)
			return
		}
		var p governanceEventPayload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Errorf("payload unmarshal: %v", err)
		}
		mu.Lock()
		payloads = append(payloads, p)
		mu.Unlock()

		status, verdict := 200, "allow"
		if onRequest != nil {
			status, verdict = onRequest(n)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status >= 200 && status < 300 {
			_, _ = w.Write([]byte(`{"governance_event_id":"ge-1","verdict":"` + verdict +
				`","risk_score":0.1,"action":"continue","fallback_used":false}`))
		} else {
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &payloads, &calls
}

func pub(t *testing.T) ed25519.PublicKey {
	return mustSigner(t).priv.Public().(ed25519.PublicKey)
}

func TestEmit_HappyPath_SignedAndParsed(t *testing.T) {
	srv, payloads, calls := coreMirrorServer(t, pub(t), nil)
	c, _ := newTestClient(t, srv.URL, false)

	v, err := c.Emit(context.Background(), sampleEvent())
	if err != nil {
		t.Fatalf("Emit returned error (should be fail-open): %v", err)
	}
	if v != VerdictAllow {
		t.Errorf("verdict = %q, want ALLOW", v)
	}
	if *calls != 1 {
		t.Errorf("calls = %d, want 1", *calls)
	}
	if len(*payloads) != 1 {
		t.Fatalf("payloads = %d, want 1", len(*payloads))
	}
	p := (*payloads)[0]
	if p.Source != source || p.EventType != "ToolCall" || p.RunID != "sess-1" {
		t.Errorf("envelope mismap: %+v", p)
	}
	if p.WorkflowID != testDID {
		t.Errorf("workflow_id = %q, want DID fallback %q", p.WorkflowID, testDID)
	}
	if p.SpanCount != 1 || len(p.Spans) != 1 || p.Spans[0].SemanticType != "file_write" {
		t.Errorf("span mismap: %+v", p.Spans)
	}
}

func TestEmit_VerdictParsing(t *testing.T) {
	cases := map[string]Verdict{
		"allow":            VerdictAllow,
		"constrain":        VerdictConstrain,
		"require_approval": VerdictRequireApproval,
		"block":            VerdictBlock,
		"halt":             VerdictHalt,
	}
	for wire, want := range cases {
		srv, _, _ := coreMirrorServer(t, pub(t), func(int) (int, string) { return 200, wire })
		c, _ := newTestClient(t, srv.URL, false)
		got, err := c.Emit(context.Background(), sampleEvent())
		if err != nil {
			t.Fatalf("Emit(%s): %v", wire, err)
		}
		if got != want {
			t.Errorf("verdict %q → %q, want %q", wire, got, want)
		}
	}
}

func TestEmit_FailOpen_Unreachable(t *testing.T) {
	// A closed port: Do() errors on every attempt. Emit must NOT error/block.
	c, log := newTestClient(t, "http://127.0.0.1:1", false)
	v, err := c.Emit(context.Background(), sampleEvent())
	if err != nil {
		t.Fatalf("fail-open violated: Emit returned error %v", err)
	}
	if v != VerdictUnknown {
		t.Errorf("verdict = %q, want unknown on drop", v)
	}
	if !strings.Contains(log.all(), "evt-1") {
		t.Errorf("expected a drop log mentioning the event id; got %q", log.all())
	}
	// INV-1: the secret key must never appear in logs.
	if strings.Contains(log.all(), testAPIKey) || strings.Contains(log.all(), testSeedB64) {
		t.Error("INV-1 violation: secret material leaked into logs")
	}
}

func TestEmit_RetriesThenSucceeds(t *testing.T) {
	// 500 on the first two attempts, 200 on the third (== maxRetries+1).
	srv, _, calls := coreMirrorServer(t, pub(t), func(n int) (int, string) {
		if n < 3 {
			return 500, ""
		}
		return 200, "allow"
	})
	c, _ := newTestClient(t, srv.URL, false)
	v, err := c.Emit(context.Background(), sampleEvent())
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if v != VerdictAllow {
		t.Errorf("verdict = %q, want ALLOW after retries", v)
	}
	if *calls != 3 {
		t.Errorf("calls = %d, want 3 (1 + 2 retries)", *calls)
	}
}

func TestEmit_5xxExhausted_FailsOpen(t *testing.T) {
	srv, _, calls := coreMirrorServer(t, pub(t), func(int) (int, string) { return 503, "" })
	c, _ := newTestClient(t, srv.URL, false)
	v, err := c.Emit(context.Background(), sampleEvent())
	if err != nil {
		t.Fatalf("fail-open violated: %v", err)
	}
	if v != VerdictUnknown {
		t.Errorf("verdict = %q, want unknown", v)
	}
	if *calls != 3 {
		t.Errorf("calls = %d, want 3 (retries exhausted)", *calls)
	}
}

func TestEmit_4xxNotRetried(t *testing.T) {
	srv, _, calls := coreMirrorServer(t, pub(t), func(int) (int, string) { return 400, "" })
	c, _ := newTestClient(t, srv.URL, false)
	v, err := c.Emit(context.Background(), sampleEvent())
	if err != nil {
		t.Fatalf("fail-open violated: %v", err)
	}
	if v != VerdictUnknown {
		t.Errorf("verdict = %q, want unknown", v)
	}
	// A 400 is terminal (e.g. today's un-accept-listed dev event_type — EXT-core).
	if *calls != 1 {
		t.Errorf("calls = %d, want 1 (4xx not retried)", *calls)
	}
}

func TestEmit_EmptyEventID_IsCallerError(t *testing.T) {
	srv, _, _ := coreMirrorServer(t, pub(t), nil)
	c, _ := newTestClient(t, srv.URL, false)
	ev := sampleEvent()
	ev.EventID = ""
	// The non-fail-open cases: preconditions the caller must fix.
	if _, err := c.Emit(context.Background(), ev); err == nil {
		t.Error("expected error for empty EventID (INV-5)")
	}
}

func TestEmit_EmptySessionID_IsCallerError(t *testing.T) {
	srv, _, _ := coreMirrorServer(t, pub(t), nil)
	c, _ := newTestClient(t, srv.URL, false)
	ev := sampleEvent()
	ev.SessionID = ""
	if _, err := c.Emit(context.Background(), ev); err == nil {
		t.Error("expected error for empty SessionID (→ core run_id)")
	}
}

func TestNew_RejectsPlaintextNonLoopback(t *testing.T) {
	base := Config{APIKey: testAPIKey, DID: testDID, SeedB64: testSeedB64}
	// Plaintext to a real host would leak the bearer key (INV-1).
	base.BaseURL = "http://core.openbox.ai"
	if _, err := New(base); err == nil {
		t.Error("expected New to reject http:// to a non-loopback host")
	}
	// https anywhere and http on loopback are allowed.
	for _, ok := range []string{"https://core.openbox.ai", "http://localhost:8080", "http://127.0.0.1:3000"} {
		base.BaseURL = ok
		if _, err := New(base); err != nil {
			t.Errorf("New(%q) unexpected error: %v", ok, err)
		}
	}
}

func TestNew_RejectsMalformedDID(t *testing.T) {
	if _, err := New(Config{BaseURL: "https://c", APIKey: testAPIKey, DID: "urn:bogus:1", SeedB64: testSeedB64}); err == nil {
		t.Error("expected New to reject a non-did:aip: DID")
	}
}

func TestEmit_RetryReusesSameEventID(t *testing.T) {
	// Idempotency (INV-5): every attempt carries the identical body, so core
	// dedupes on the same event's metadata/ids rather than double-counting.
	srv, payloads, _ := coreMirrorServer(t, pub(t), func(n int) (int, string) {
		if n < 2 {
			return 500, ""
		}
		return 200, "allow"
	})
	c, _ := newTestClient(t, srv.URL, false)
	ev := sampleEvent()
	ev.Metadata = map[string]any{"marker": "m1"}
	if _, err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(*payloads) != 2 {
		t.Fatalf("payloads seen = %d, want 2", len(*payloads))
	}
	if string((*payloads)[0].Metadata) != string((*payloads)[1].Metadata) {
		t.Error("retry body differed from original — idempotency broken")
	}
}
