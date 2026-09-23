package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayemit"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway/gatewaytest"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

// TestSpooledTransportEventReachesTheWire closes the one lane whose bodies were
// never graded on the wire.
//
// The gateway lane had this and the proxy lane did not, and the two share the
// mapping code -- which is exactly the reasoning the evidence axis exists to
// stop being invisible. This drives a model call through the real CONNECT and
// TLS chain, spools it, emits it, and reads the request and response bodies
// back off the captured wire body.
//
// It also asserts a tool-telemetry call egresses no body at all. That property
// is real, but note what it does NOT prove: removing WithBodyCapture entirely
// changes neither assertion, because a nil predicate means capture-everything
// at the relay AND the emitter classifies the path a second time when it builds
// the event (gatewayemit/event.go). The body is gated twice, so the transport
// predicate is a redundant outer gate and nothing observable here depends on it
// being installed. Measured, not assumed: with the option dropped, a telemetry
// call still spools no body.
//
// So the production predicate in cmd/openbox/transport.go stays unexercised,
// and docs/coverage.md says so rather than letting this test imply otherwise.
func TestSpooledTransportEventReachesTheWire(t *testing.T) {
	const requestMarker = "MARKER-request-body-through-the-proxy-lane"
	const responseMarker = "MARKER-response-body-through-the-proxy-lane"
	const telemetryMarker = "MARKER-tool-telemetry-body-must-never-egress"
	const sessionID = "7f3c9b2e-0000-5000-a000-00000000ab1e"

	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":%q}]}`, responseMarker)
	}))
	t.Cleanup(upstream.Close)
	gatewaytest.SwapUpstreamDial(t, memhttptest.DialContext)

	spoolDir := t.TempDir()
	warn := &warnLog{}
	ca, err := transport.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	em := &gatewayemit.Emitter{
		Elected: func() bool { return true },
		Lane:    gatewayemit.LaneProxy,
		Deliver: testSpoolDeliver(spoolDir),
		DID:     func() string { return "did:aip:7f3c9b2e-0000-5000-a000-00000000feed" },
		Warn:    warn.record,
	}

	// The predicate the binary installs, not one written for the test.
	p, err := transport.New(transport.Config{Upstream: upstream.URL}, ca, em,
		transport.WithBodyCapture(func(r *http.Request) bool {
			return gatewayemit.CapturesBody(r.URL.Path)
		}))
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}

	conn := connectAndHandshake(t, p, ca, "api.anthropic.com")
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages",
		strings.NewReader(fmt.Sprintf(`{"model":"claude-x","messages":[{"role":"user","content":%q}]}`, requestMarker)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claude-Code-Session-Id", sessionID)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	// Closed before the spool is read: the relay records the call when the
	// response is complete, so reading earlier races the thing under test.
	resp.Body.Close()
	// The relay must have carried the response through unchanged, or the lane
	// captured something the developer never received.
	if !strings.Contains(string(body), responseMarker) {
		t.Fatalf("the relayed response did not reach the caller; the chain is broken before the capture")
	}

	// The emit is asynchronous, so the spool has to be waited for rather than
	// read once -- reading immediately races the thing under test and reports
	// an empty spool as a capture failure.
	waitForSpool(t, spoolDir, sessionID, warn)

	// Both halves, not the completed one alone: the request body rides the
	// started half, so keeping only the completion would assert exactly half of
	// what this test is for and silently pass if the other half never went.
	events := readBothSpooledHalves(t, spoolDir)

	fake := fakecore.New(t, fakecore.Script{})
	c, err := client.New(client.Config{
		BaseURL:               fake.URL(),
		APIKey:                fakecore.APIKey(),
		WorkloadPrivateKey:    fakecore.WorkloadPrivateKey(),
		ContentCaptureEnabled: true,
	})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	for i, ev := range events {
		if _, err := c.Emit(t.Context(), ev); err != nil {
			t.Fatalf("emit spooled event #%d: %v", i, err)
		}
	}
	if refused := fake.Rejections(); len(refused) > 0 {
		t.Fatalf("the fake refused a relayed event: %s", strings.Join(refused, " | "))
	}

	var sawRequest, sawResponse bool
	for _, r := range fake.Inbox() {
		if fieldContains(r, "activity_input", requestMarker) {
			sawRequest = true
		}
		if fieldContains(r, "activity_output", responseMarker) {
			sawResponse = true
		}
		if strings.Contains(string(r.Raw), telemetryMarker) {
			t.Errorf("a tool-telemetry body egressed on a %s row. The tool reporting on itself "+
				"is classified as carrying no content, at the relay and again at the emitter; "+
				"both would have to have stopped agreeing for this to fire.", r.EventType())
		}
	}
	if !sawRequest {
		t.Error("the relayed request body never reached activity_input on the wire; " +
			"the lane records a model call with no request to show for it")
	}
	if !sawResponse {
		t.Error("the relayed response body never reached activity_output on the wire")
	}
}

// relay drives one request through the CONNECT and TLS chain and waits for the
// response to complete, which is when the lane records the call.
func relay(t *testing.T, p *transport.Proxy, ca *transport.CA, sessionID, path, reqBody, wantInResponse string) {
	t.Helper()
	conn := connectAndHandshake(t, p, ca, "api.anthropic.com")
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com"+path, strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claude-Code-Session-Id", sessionID)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	resp.Body.Close()
	// The relay must have carried the response through unchanged, or the lane
	// captured something the developer never received.
	if !strings.Contains(string(body), wantInResponse) {
		t.Fatalf("%s: the relayed response did not reach the caller; the chain is broken before the capture", path)
	}
}

func fieldContains(r fakecore.Received, field, want string) bool {
	v, ok := r.Body[field]
	if !ok {
		return false
	}
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), want)
}

// readBothSpooledHalves returns the pair a relayed call produces, in append
// order. readOneSpooledEvent returns the completion alone, which is the right
// answer for a caller asking "what happened" and the wrong one here.
func readBothSpooledHalves(t *testing.T, spoolDir string) []client.DevEvent {
	t.Helper()
	entries, err := os.ReadDir(spoolDir)
	if err != nil {
		t.Fatalf("read spool dir: %v", err)
	}
	var out []client.DevEvent
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		raw, err := os.ReadFile(spoolDir + "/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var ev client.DevEvent
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("unmarshal spooled event: %v", err)
			}
			out = append(out, ev)
		}
	}
	if len(out) < 2 {
		t.Fatalf("spool holds %d events, want at least the Started/Completed pair; the relay "+
			"answered, so a break here is between the capture and the spool", len(out))
	}
	return out
}
