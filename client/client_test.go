package client

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
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
	if v.Verdict != VerdictAllow {
		t.Errorf("verdict = %q, want ALLOW", v.Verdict)
	}
	if *calls != 1 {
		t.Errorf("calls = %d, want 1", *calls)
	}
	if len(*payloads) != 1 {
		t.Fatalf("payloads = %d, want 1", len(*payloads))
	}
	p := (*payloads)[0]
	// E7-S4: a ToolCall serializes as an ActivityStarted hook (NOT event_type
	// "ToolCall") — the base SDK's flat hook wire shape.
	if p.Source != source || p.EventType != "ActivityStarted" || p.RunID != "sess-1" {
		t.Errorf("envelope mismap: %+v", p)
	}
	if p.WorkflowID != testDID {
		t.Errorf("workflow_id = %q, want DID fallback %q", p.WorkflowID, testDID)
	}
	// One flat span, stage=started, Core-classifiable file name + path; the client
	// no longer sends semantic_type (Core computes it).
	if p.SpanCount != 1 || len(p.Spans) != 1 {
		t.Fatalf("span mismap: %+v", p.Spans)
	}
	if s := p.Spans[0]; s.Stage != "started" || s.Name != "file.write" || s.SemanticType != "" {
		t.Errorf("hook span mismap: %+v", s)
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
		if got.Verdict != want {
			t.Errorf("verdict %q → %q, want %q", wire, got.Verdict, want)
		}
	}
}

func TestEmit_FailOpen_Unreachable(t *testing.T) {
	// A closed port: Do() errors on every attempt. The fail-open invariant is
	// about the VERDICT, not the error: a transport failure must yield a verdict
	// no caller can read as a block. Emit also reports ErrDelivery so a durable
	// caller can retry instead of losing the event (E8-S7) — advisory only.
	c, log := newTestClient(t, "http://127.0.0.1:1", false)
	v, err := c.Emit(context.Background(), sampleEvent())
	if !errors.Is(err, ErrDelivery) {
		t.Fatalf("want ErrDelivery so the spool can retry, got %v", err)
	}
	if v.Verdict != VerdictUnknown {
		t.Errorf("fail-open violated: verdict = %q, want unknown on a delivery failure", v.Verdict)
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
	if v.Verdict != VerdictAllow {
		t.Errorf("verdict = %q, want ALLOW after retries", v.Verdict)
	}
	if *calls != 3 {
		t.Errorf("calls = %d, want 3 (1 + 2 retries)", *calls)
	}
}

func TestEmit_5xxExhausted_FailsOpen(t *testing.T) {
	srv, _, calls := coreMirrorServer(t, pub(t), func(int) (int, string) { return 503, "" })
	c, _ := newTestClient(t, srv.URL, false)
	v, err := c.Emit(context.Background(), sampleEvent())
	if !errors.Is(err, ErrDelivery) {
		t.Fatalf("want ErrDelivery so the spool can retry, got %v", err)
	}
	if v.Verdict != VerdictUnknown {
		t.Errorf("fail-open violated: verdict = %q, want unknown", v.Verdict)
	}
	if *calls != 3 {
		t.Errorf("calls = %d, want 3 (retries exhausted)", *calls)
	}
}

func TestEmit_4xxNotRetried(t *testing.T) {
	srv, _, calls := coreMirrorServer(t, pub(t), func(int) (int, string) { return 400, "" })
	c, _ := newTestClient(t, srv.URL, false)
	v, err := c.Emit(context.Background(), sampleEvent())
	if !errors.Is(err, ErrDelivery) {
		t.Fatalf("want ErrDelivery so the spool can retry, got %v", err)
	}
	if v.Verdict != VerdictUnknown {
		t.Errorf("fail-open violated: verdict = %q, want unknown", v.Verdict)
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

// TestEmit_IdempotencyKeyHeader (STORY-SL-14): every POST carries an
// Idempotency-Key request header equal to the event's EventID and to the
// metadata.event_id in the signed body — the explicit, header-standard half of
// the dedupe contract (inert until EXT-core consumes it).
func TestEmit_IdempotencyKeyHeader(t *testing.T) {
	var mu sync.Mutex
	var headers []string
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		headers = append(headers, r.Header.Get("Idempotency-Key"))
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"verdict":"allow"}`))
	}))
	t.Cleanup(srv.Close)

	c, _ := newTestClient(t, srv.URL, false)
	ev := sampleEvent()
	ev.EventID = "cc-abc123"
	if _, err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(headers) != 1 {
		t.Fatalf("calls = %d, want 1", len(headers))
	}
	if headers[0] != ev.EventID {
		t.Errorf("Idempotency-Key = %q, want %q (== EventID)", headers[0], ev.EventID)
	}
	// And it must equal metadata.event_id in the signed body.
	var p governanceEventPayload
	if err := json.Unmarshal(bodies[0], &p); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(p.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if meta["event_id"] != ev.EventID {
		t.Errorf("metadata.event_id = %v, want %q", meta["event_id"], ev.EventID)
	}
	if headers[0] != meta["event_id"] {
		t.Errorf("Idempotency-Key %q != metadata.event_id %v", headers[0], meta["event_id"])
	}
}

// TestEmit_IdempotencyKeyStableAcrossRetries (STORY-SL-14): a retry after a lost
// 200 (modeled as a 500 then 200) re-sends the SAME Idempotency-Key — never a
// freshly generated one — so an eventual server-side dedupe collapses the two
// stored copies. This is the client half of the lost-200 delivery guarantee.
func TestEmit_IdempotencyKeyStableAcrossRetries(t *testing.T) {
	var mu sync.Mutex
	var headers []string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		headers = append(headers, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		if n < 2 {
			w.WriteHeader(500) // lost-200 stand-in: server stored it, client saw failure
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"verdict":"allow"}`))
	}))
	t.Cleanup(srv.Close)

	c, _ := newTestClient(t, srv.URL, false)
	ev := sampleEvent()
	ev.EventID = "cc-retry-me"
	if _, err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(headers) != 2 {
		t.Fatalf("calls = %d, want 2 (1 + 1 retry)", len(headers))
	}
	if headers[0] != ev.EventID || headers[1] != ev.EventID {
		t.Errorf("Idempotency-Key not stable across retry: %q then %q, want %q both",
			headers[0], headers[1], ev.EventID)
	}
}
