package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"

	"github.com/openbox-ai/openbox-shift-left/client/memhttptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// TestGatewayCommandActuallyCaptures is the seam test.
//
// The gateway shipped for a while with capture UNWIRED: package gateway's
// WithCapture had no production caller at all, so g.emitter was nil, every
// captured exchange was discarded, and both sides stayed green — the relay was
// exercised against a stub Emitter, and the span builder against a hand-written
// DevEvent. A fake at each end of a seam that has no implementation between them
// proves nothing about the seam.
//
// So this test supplies NO fake. It drives the real `openbox gateway` command,
// through the real relay, into the real emitter and the real spool, and reads the
// event back off disk. It fails if any link in that chain is missing.
func TestGatewayCommandActuallyCaptures(t *testing.T) {
	memhttptest.RequireBind(t)
	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Request-Id", "req_upstream_seam")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","role":"assistant","content":[]}`)
	}))
	defer upstream.Close()

	spoolDir := t.TempDir()
	t.Setenv("OPENBOX_SPOOL_DIR", spoolDir)
	t.Setenv("OPENBOX_AGENT_DID", "did:aip:7f3c9b2e-0000-5000-a000-00000000feed")
	// The detached flusher must never be spawned from a test binary.
	t.Setenv("OPENBOX_REALTIME", "0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan net.Addr, 1)
	a, _, errb := testApp(nil)
	a.gatewayCtx = ctx
	a.gatewayReady = func(addr net.Addr) { ready <- addr }

	done := make(chan int, 1)
	go func() { done <- a.runGateway([]string{"--addr", "127.0.0.1:0", "--upstream", upstream.URL}) }()

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

	raw := readOneSpooledLine(t, spoolDir)
	ev := decodeSpooledEvent(t, raw)
	if ev.EventType != client.EventTurnCompleted {
		t.Errorf("EventType = %q, want TurnCompleted (nothing else attaches the span)", ev.EventType)
	}
	if ev.SessionID != "seam-session" {
		t.Errorf("SessionID = %q, want the id the request header named", ev.SessionID)
	}
	if ev.GatewayRequestID != "req_upstream_seam" {
		t.Errorf("GatewayRequestID = %q, want the provider's own request id", ev.GatewayRequestID)
	}
	if ev.Span == nil {
		t.Fatal("no span — the observed exchange was not recorded")
	}
	if ev.Span.HTTPStatus != 200 || ev.Span.HTTPMethod != "POST" {
		t.Errorf("classification fields wrong: %s %d", ev.Span.HTTPMethod, ev.Span.HTTPStatus)
	}
	if !strings.Contains(ev.Span.RequestBody, "claude-opus-4") {
		t.Errorf("request body not captured: %q", ev.Span.RequestBody)
	}
	if !strings.Contains(ev.Span.ResponseBody, "assistant") {
		t.Errorf("response body not captured: %q", ev.Span.ResponseBody)
	}
	if ev.Span.CredentialFingerprint == "" {
		t.Error("no credential fingerprint — account binding would have nothing to match")
	}
	// The credential must be nowhere in the spooled record, and this is asserted
	// against the RAW LINE rather than one decoded map key: a leak through any
	// other field — a body, an attribute, a future field nobody thought about —
	// has to turn this red too.
	//
	// The spool is a plaintext file on disk, so this is a check at rest, not only
	// on the wire.
	if strings.Contains(string(raw), fakeCredential()) {
		t.Error("the raw provider credential was written to the spool")
	}
	// And the redaction has to be the REASON it is absent, not an accident of
	// serialization: the key survives with a redacted value, so a reviewer can
	// still see that a credential was sent.
	if got := ev.Span.RequestHeaders["Authorization"]; got != "[redacted]" {
		t.Errorf("Authorization = %q, want exactly %q", got, "[redacted]")
	}
}

// fakeCredential builds a stand-in provider credential AT RUNTIME.
//
// It is assembled from fragments on purpose. A credential-shaped literal in this
// file would be rewritten by this repo's own enforce-path redactor before the
// file was ever written to disk — and the absence assertion would then be
// checking for a string the test never sent, passing for a reason that has
// nothing to do with the gateway. That trap has caught this repo before; it
// caught this test too, in its first version.
//
// Low entropy deliberately: the body redactor must not rewrite it either, or the
// absence check goes vacuous a second way.
func fakeCredential() string {
	return "sk" + "-ant-" + strings.Repeat("q7", 20)
}

// TestGatewayWithoutADIDStillRelays keeps the governance sensor from becoming a
// precondition for the developer's model calls. An unconfigured machine must
// still get a working relay — the same fail-open direction the hook path holds.
func TestGatewayWithoutADIDStillRelays(t *testing.T) {
	memhttptest.RequireBind(t)
	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_AGENT_DID", "")
	t.Setenv("OPENBOX_CONFIG", filepath.Join(t.TempDir(), "absent.json"))
	t.Setenv("OPENBOX_REALTIME", "0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	a, _, errb := testApp(nil)
	a.gatewayCtx = ctx
	a.gatewayReady = func(addr net.Addr) { ready <- addr }

	done := make(chan int, 1)
	go func() { done <- a.runGateway([]string{"--addr", "127.0.0.1:0", "--upstream", upstream.URL}) }()

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

// readOneSpooledLine returns the first spooled line's raw bytes. Raw, because the
// credential-absence assertion has to see everything that was written, not the
// subset a struct happens to model.
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
	t.Fatalf("no event was spooled — capture is not wired into the gateway command (files: %v)", entries)
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

// TestSpooledGatewayEventReachesTheWire closes the last seam a fake could hide.
//
// TestGatewayCommandActuallyCaptures proves capture reaches the spool; the client
// package proves an in-memory event reaches the wire. Between them sits the part
// neither covers: the spooled LINE a daemon wrote, read back by a separate flush
// process, rebuilt into a payload and signed. This drives the real dispatcher for
// both halves and asserts the POSTed bytes.
//
// It also pins something no other test can: the "cc-spool" directory and
// "claude-code" provider that gateway.go names must be the SAME ones the adapter's
// flush drains. If those drift, the flush finds nothing and this reds — whereas
// every unit test on either side would stay green.
func TestSpooledGatewayEventReachesTheWire(t *testing.T) {
	memhttptest.RequireBind(t)
	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Request-Id", "req_wire_seam")
		io.WriteString(w, `{"type":"message","role":"assistant"}`)
	}))
	defer upstream.Close()

	var posted []byte
	core := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"decision":"ALLOW"}`)
	}))
	defer core.Close()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_AGENT_DID", "did:aip:7f3c9b2e-0000-5000-a000-000000000w1r")
	t.Setenv("OPENBOX_API_KEY", "obx_test")
	t.Setenv("OPENBOX_AGENT_PRIVATE_KEY", base64.StdEncoding.EncodeToString(priv.Seed()))
	t.Setenv("OPENBOX_BASE_URL", core.URL)
	t.Setenv("OPENBOX_CONTENT_CAPTURE", "1")
	t.Setenv("OPENBOX_REALTIME", "0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	a, _, errb := testApp(nil)
	a.gatewayCtx = ctx
	a.gatewayReady = func(addr net.Addr) { ready <- addr }

	done := make(chan int, 1)
	go func() { done <- a.runGateway([]string{"--addr", "127.0.0.1:0", "--upstream", upstream.URL}) }()

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

	// The real flush path, through the real dispatcher.
	t.Setenv("OPENBOX_FLUSH_SESSION", "wire-seam-session")
	flush, _, flushErr := testApp(nil)
	if code := flush.run([]string{"hook", "claude-code", "flush"}); code != exitOK {
		t.Fatalf("flush exited %d: %s", code, flushErr.String())
	}
	if len(posted) == 0 {
		t.Fatal("the flush delivered nothing — the gateway's spool and the adapter's flush do not agree on a location")
	}

	var p struct {
		ActivityID string `json:"activity_id"`
		SpanCount  int    `json:"span_count"`
		Spans      []struct {
			SemanticType string         `json:"semantic_type"`
			HTTPStatus   int            `json:"http_status_code"`
			RequestBody  string         `json:"request_body"`
			Attributes   map[string]any `json:"attributes"`
		} `json:"spans"`
	}
	if err := json.Unmarshal(posted, &p); err != nil {
		t.Fatalf("unmarshal posted payload: %v", err)
	}
	if p.ActivityID != "wire-seam-session:gateway:req_wire_seam" {
		t.Errorf("activity_id = %q, want the gateway namespace", p.ActivityID)
	}
	if p.SpanCount != 1 || len(p.Spans) != 1 {
		t.Fatalf("span_count=%d — the observed exchange did not survive the spool round-trip", p.SpanCount)
	}
	if p.Spans[0].SemanticType != "llm_completion" {
		t.Errorf("semantic_type = %q; core's readers filter on llm_completion", p.Spans[0].SemanticType)
	}
	if p.Spans[0].HTTPStatus != 200 {
		t.Errorf("http_status_code = %d", p.Spans[0].HTTPStatus)
	}
	if !strings.Contains(p.Spans[0].RequestBody, "claude-opus-4") {
		t.Errorf("request body lost between spool and wire: %q", p.Spans[0].RequestBody)
	}
	if p.Spans[0].Attributes["openbox.credential_fingerprint"] == nil {
		t.Error("fingerprint absent from attributes — account binding has no route into core")
	}
	if strings.Contains(string(posted), fakeCredential()) {
		t.Error("the raw provider credential reached the wire")
	}
}
