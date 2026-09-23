package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"

	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"github.com/openbox-ai/openbox-shift-left/internal/client/workloadauth"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// TestGatewayCommandActuallyCaptures is the seam test. A fake at each end of a
// seam that has no implementation between them proves nothing about the seam.
func TestGatewayCommandActuallyCaptures(t *testing.T) {
	memhttptest.RequireBind(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Request-Id", "req_upstream_seam")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","role":"assistant","content":[]}`)
	}))
	defer upstream.Close()

	spoolDir := t.TempDir()
	t.Setenv("OPENBOX_SPOOL_DIR", spoolDir)
	t.Setenv(devconfig.EnvAgentID, "7f3c9b2e-0000-5000-a000-00000000feed")
	t.Setenv("OPENBOX_REALTIME", "0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan net.Addr, 1)
	a, _, errb := testApp(nil)
	a.gatewayCtx = ctx
	a.gatewayReady = func(addr net.Addr) { ready <- addr }

	done := make(chan int, 1)
	go func() {
		done <- a.runGateway([]string{"--addr", "127.0.0.1:0", "--upstream", upstream.URL, "--elected"})
	}()

	var addr net.Addr
	select {
	case addr = <-ready:
	case <-time.After(10 * time.Second):
		t.Fatalf("gateway never bound a listener; stderr: %s", errb.String())
	}

	req, err := http.NewRequest(http.MethodPost, "http://"+addr.String()+"/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer "+fakeCredential())
	req.Header.Set("X-Claude-Code-Session-Id", "seam-session")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("relay request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("relay returned %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(45 * time.Second):
		t.Fatal("runGateway did not return after its context ended")
	}

	// The COMPLETED half: one relayed call is one activity with two rows, and this
	// is the one carrying the response and the duration.
	raw := readSpooledLine(t, spoolDir, client.EventTurnCompleted)
	ev := decodeSpooledEvent(t, raw)
	if ev.EventType != client.EventTurnCompleted {
		t.Errorf("EventType = %q, want TurnCompleted", ev.EventType)
	}
	if ev.SessionID != "seam-session" {
		t.Errorf("SessionID = %q, want the id the request header named", ev.SessionID)
	}
	if ev.GatewayRequestID != "req_upstream_seam" {
		t.Errorf("GatewayRequestID = %q, want the provider's own request id", ev.GatewayRequestID)
	}
	if ev.Span == nil {
		t.Fatal("no span; the observed exchange was not recorded")
	}
	if ev.Span.HTTPStatus != 200 || ev.Span.HTTPMethod != "POST" {
		t.Errorf("classification fields wrong: %s %d", ev.Span.HTTPMethod, ev.Span.HTTPStatus)
	}
	// The response on the closing half; the REQUEST is asserted on the opening half
	// below, because that is the only half that carries it. Duplicating it here
	// doubled the spooled bytes for a value nothing reads.
	if !strings.Contains(ev.Span.ResponseBody, "assistant") {
		t.Errorf("response body not captured: %q", ev.Span.ResponseBody)
	}
	if ev.Span.CredentialFingerprint == "" {
		t.Error("no credential fingerprint; account binding would have nothing to match")
	}
	if strings.Contains(string(raw), fakeCredential()) {
		t.Error("the raw provider credential was written to the spool")
	}
	// The redaction has to be the REASON it is absent, not an accident of
	// serialization: the key survives with a redacted value, so a reviewer can
	// still see that a credential was sent.
	// Asserted on the CAPTURE rather than on the wire, because the observed
	// headers no longer egress: they only ever reached core inside spans[], which
	// the normal path parses and discards, and they are the highest-risk class
	// this client handles since the developer's live credential is on every model
	// request. The redaction still has to be the REASON the value is absent, not
	// an accident of serialization, so the key survives with a redacted value.
	if got := ev.Span.RequestHeaders["Authorization"]; got != "" {
		t.Errorf("Authorization = %q; the headers must not reach the spool at all now", got)
	}
	if strings.Contains(string(raw), "Anthropic-Version") {
		t.Errorf("observed headers reached the spool: %s", raw)
	}

	// Both halves are there, sharing one request id.
	started := decodeSpooledEvent(t, readSpooledLine(t, spoolDir, client.EventTurnStarted))
	if started.GatewayRequestID != ev.GatewayRequestID {
		t.Errorf("the pair split across request ids (%q, %q)", started.GatewayRequestID, ev.GatewayRequestID)
	}
	if !strings.Contains(started.Span.RequestBody, "claude-opus-4") {
		t.Errorf("the opening half did not carry the request body: %q", started.Span.RequestBody)
	}
}

// fakeCredential builds a stand-in provider credential AT runtime. It is
// assembled from fragments on purpose.
func fakeCredential() string {
	return "sk" + "-ant-" + strings.Repeat("q7", 20)
}

// TestGatewayWithoutADIDStillRelays keeps the governance sensor from becoming
// a precondition for the developer's model calls. An unconfigured machine must
// still get a working relay; the same fail-open direction the hook path holds.
func TestGatewayWithoutADIDStillRelays(t *testing.T) {
	memhttptest.RequireBind(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv(devconfig.EnvAgentID, "")
	t.Setenv("OPENBOX_CONFIG", filepath.Join(t.TempDir(), "absent.json"))
	t.Setenv("OPENBOX_REALTIME", "0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	a, _, errb := testApp(nil)
	a.gatewayCtx = ctx
	a.gatewayReady = func(addr net.Addr) { ready <- addr }

	done := make(chan int, 1)
	go func() {
		done <- a.runGateway([]string{"--addr", "127.0.0.1:0", "--upstream", upstream.URL, "--elected"})
	}()

	var addr net.Addr
	select {
	case addr = <-ready:
	case <-time.After(10 * time.Second):
		t.Fatalf("gateway never bound; stderr: %s", errb.String())
	}
	resp, err := http.Post("http://"+addr.String()+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("relay failed with no DID configured: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("relay returned %d with no DID configured, want 200", resp.StatusCode)
	}
	cancel()
	<-done
}

// readSpooledLine returns the spooled line for one half of the pair. It replaced
// a helper that took the FIRST line, which stopped being unambiguous when one
// relayed call began producing two rows.
func readSpooledLine(t *testing.T, dir string, want client.EventType) []byte {
	t.Helper()
	for _, line := range readSpooledLines(t, dir) {
		var probe struct {
			EventType client.EventType `json:"event_type"`
		}
		if json.Unmarshal(line, &probe) == nil && probe.EventType == want {
			return line
		}
	}
	t.Fatalf("no spooled %s event in %s", want, dir)
	return nil
}

// readSpooledLines returns every spooled line across every file in the spool.
func readSpooledLines(t *testing.T, dir string) [][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read spool dir: %v", err)
	}
	var out [][]byte
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("open spool file: %v", err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
		for sc.Scan() {
			line := make([]byte, len(sc.Bytes()))
			copy(line, sc.Bytes())
			out = append(out, line)
		}
		f.Close()
	}
	return out
}

func readOneSpooledLine(t *testing.T, dir string) []byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read spool dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("open spool file: %v", err)
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
		for sc.Scan() {
			line := make([]byte, len(sc.Bytes()))
			copy(line, sc.Bytes())
			return line
		}
	}
	t.Fatalf("no event was spooled; capture is not wired into the gateway command (files: %v)", entries)
	return nil
}

func decodeSpooledEvent(t *testing.T, line []byte) client.DevEvent {
	t.Helper()
	var ev client.DevEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		t.Fatalf("spool line is not a DevEvent: %v", err)
	}
	return ev
}

// TestSpooledGatewayEventReachesTheWire closes the last seam a fake could
// hide. TestGatewayCommandActuallyCaptures proves capture reaches the spool;
// the client package proves an in-memory event reaches the wire.
func TestSpooledGatewayEventReachesTheWire(t *testing.T) {
	memhttptest.RequireBind(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Request-Id", "req_wire_seam")
		io.WriteString(w, `{"type":"message","role":"assistant"}`)
	}))
	defer upstream.Close()

	// fakecore, not a hand-rolled catch-all: the client now speaks the v3
	// bootstrap/exchange dance before it ever reaches /evaluate, and a
	// single-path handler answering every method has no bootstrap document or
	// token endpoint to offer.
	core := fakecore.New(t, fakecore.Script{})

	home := t.TempDir()
	t.Setenv(devconfig.EnvHome, home)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv(devconfig.EnvAgentID, fakecore.AgentID())
	t.Setenv(devconfig.EnvAPIKeyDirect, fakecore.APIKey())
	workloadKey, err := workloadauth.NormalizePrivateKey(fakecore.WorkloadPrivateKey())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(devconfig.EnvWorkloadPrivateKey, workloadKey)
	t.Setenv("OPENBOX_BASE_URL", core.URL())
	t.Setenv("OPENBOX_CONTENT_CAPTURE", "1")
	t.Setenv("OPENBOX_REALTIME", "0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	a, _, errb := testApp(nil)
	a.gatewayCtx = ctx
	a.gatewayReady = func(addr net.Addr) { ready <- addr }

	done := make(chan int, 1)
	go func() {
		done <- a.runGateway([]string{"--addr", "127.0.0.1:0", "--upstream", upstream.URL, "--elected"})
	}()

	var addr net.Addr
	select {
	case addr = <-ready:
	case <-time.After(10 * time.Second):
		t.Fatalf("gateway never bound; stderr: %s", errb.String())
	}

	req, _ := http.NewRequest(http.MethodPost, "http://"+addr.String()+"/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4"}`))
	req.Header.Set("X-Claude-Code-Session-Id", "wire-seam-session")
	req.Header.Set("Authorization", "Bearer "+fakeCredential())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	cancel()
	<-done

	t.Setenv("OPENBOX_FLUSH_SESSION", "wire-seam-session")
	flush, _, flushErr := testApp(nil)
	if code := flush.run([]string{"hook", "claude-code", "flush"}); code != exitOK {
		t.Fatalf("flush exited %d: %s", code, flushErr.String())
	}
	inbox := core.Inbox()
	postedAll := make([][]byte, len(inbox))
	for i, r := range inbox {
		postedAll[i] = r.Raw
	}
	if len(postedAll) == 0 {
		t.Fatal("the flush delivered nothing; the gateway's spool and the adapter's flush do not agree on a location")
	}

	// Every posted payload, because one relayed call is now two rows and the
	// evidence is split across them by design: the request opens the activity and
	// the response closes it.
	type payload struct {
		EventType      string          `json:"event_type"`
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
	var halves []payload
	for _, raw := range postedAll {
		var p payload
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("unmarshal posted payload: %v", err)
		}
		halves = append(halves, p)
	}
	if len(halves) != 2 {
		t.Fatalf("the flush posted %d payload(s), want the pair", len(halves))
	}

	var sawRequest, sawDuration bool
	for _, p := range halves {
		if p.ActivityID != "wire-seam-session:gateway:req_wire_seam" {
			t.Errorf("%s: activity_id = %q, want the gateway namespace", p.EventType, p.ActivityID)
		}
		if p.ActivityType != client.ActivityTypeLLMCompletion {
			t.Errorf("%s: activity_type = %q; core's readers filter on llm_completion", p.EventType, p.ActivityType)
		}
		// Rehomed from the span's attributes, which never persisted at all for a
		// developer session, so account binding has never had a route into core.
		if p.Metadata.CredentialFingerprint == "" {
			t.Errorf("%s: fingerprint absent from metadata; account binding has no route into core", p.EventType)
		}
		if p.SpanCount != 0 || len(p.Spans) != 0 {
			t.Errorf("%s: span_count=%d spans=%d, want none", p.EventType, p.SpanCount, len(p.Spans))
		}
		if strings.Contains(string(p.ActivityInput), "claude-opus-4") {
			sawRequest = true
		}
		if p.DurationMs != nil {
			sawDuration = true
		}
	}
	if !sawRequest {
		t.Errorf("the request body was lost between spool and wire: %s", postedAll)
	}
	if !sawDuration {
		t.Errorf("no half carried duration_ms; the relay measured the call: %s", postedAll)
	}
	for _, raw := range postedAll {
		if strings.Contains(string(raw), fakeCredential()) {
			t.Error("the raw provider credential reached the wire")
		}
	}
}
