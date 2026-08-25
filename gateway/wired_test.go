package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingEmitter collects what the gateway reported.
type recordingEmitter struct {
	mu   sync.Mutex
	seen []Captured
}

func (e *recordingEmitter) Emit(_ context.Context, c Captured) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, c)
}

func (e *recordingEmitter) all() []Captured {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Captured(nil), e.seen...)
}

// await waits for n captures.
//
// Necessary, not defensive: the client returns from reading the body when the
// server closes the response, which is BEFORE the handler goroutine has run the
// emit that follows the relay. Asserting immediately after the client sees EOF is a
// race on a server-side side effect, and it is one this test lost on the first run.
func (e *recordingEmitter) await(t *testing.T, n int) []Captured {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := e.all(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := e.all()
	t.Fatalf("emitted %d captures, want %d", len(got), n)
	return got
}

// wire builds a gateway with capture and/or the gate turned on.
func wire(t *testing.T, upstream string, em Emitter, ev Evaluator, gated func(*http.Request) bool) *Gateway {
	t.Helper()
	g, err := New(Config{Upstream: upstream})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if em != nil {
		g = g.WithCapture(em)
	}
	if ev != nil {
		g = g.WithGate(ev, gated)
	}
	return g
}

// TestRelayIsByteIdenticalWithCaptureOn is the regression that matters most.
//
// Capture changes what is REPORTED, never what is forwarded. If turning it on
// altered a single forwarded byte, the invariant this whole package exists for
// would hold only in the configuration nobody runs in production.
func TestRelayIsByteIdenticalWithCaptureOn(t *testing.T) {
	var withCapture, without recorded

	for _, tc := range []struct {
		name string
		got  *recorded
		em   Emitter
	}{
		{"capture on", &withCapture, &recordingEmitter{}},
		{"capture off", &without, nil},
	} {
		upstream := upstreamRecorder(t, tc.got, nil)
		gw := serveGateway(t, wire(t, upstream.URL, tc.em, nil, nil))

		req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages?beta=true", strings.NewReader(fixtureBody))
		req.Header.Set("Authorization", fixtureCredential)
		req.Header.Set("X-Api-Key", fixtureAPIKey)
		req.Header.Set("Anthropic-Beta", "some-unreleased-beta-2099-01-01")
		resp, err := probeClient().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		resp.Body.Close()
	}

	if string(withCapture.body) != string(without.body) {
		t.Errorf("capture altered the forwarded body:\n on: %q\noff: %q", withCapture.body, without.body)
	}
	if withCapture.target != without.target {
		t.Errorf("capture altered the request target: %q vs %q", withCapture.target, without.target)
	}
	// The credentials in particular must relay untouched with capture on — the
	// capture copy is redacted, the forward is not.
	if withCapture.header.Get("Authorization") != fixtureCredential {
		t.Errorf("capture redacted the FORWARDED Authorization: %q", withCapture.header.Get("Authorization"))
	}
	if withCapture.header.Get("X-Api-Key") != fixtureAPIKey {
		t.Errorf("capture redacted the FORWARDED x-api-key: %q", withCapture.header.Get("X-Api-Key"))
	}
	if withCapture.header.Get("Anthropic-Beta") != without.header.Get("Anthropic-Beta") {
		t.Error("capture altered a forwarded header")
	}
}

// TestCaptureReportsRedactedEvidence is the other side: the copy that gets
// reported has the credential removed and the fingerprint present.
func TestCaptureReportsRedactedEvidence(t *testing.T) {
	var got recorded
	upstream := upstreamRecorder(t, &got, func(w http.ResponseWriter) {
		w.Header().Set("Request-Id", "req_up")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"type":"message","role":"assistant"}`)
	})
	em := &recordingEmitter{}
	gw := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages?beta=true", strings.NewReader(fixtureBody))
	req.Header.Set("Authorization", fixtureCredential)
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	seen := em.await(t, 1)
	c := seen[0]

	if c.RequestHeaders["Authorization"] != redactedHeaderValue {
		t.Errorf("reported Authorization was not redacted: %q", c.RequestHeaders["Authorization"])
	}
	if c.CredentialFingerprint == "" {
		t.Error("no fingerprint on the reported evidence")
	}
	if c.HTTPStatus != http.StatusOK || c.HTTPMethod != http.MethodPost {
		t.Errorf("classification fields wrong: %d %q", c.HTTPStatus, c.HTTPMethod)
	}
	if strings.Contains(c.HTTPURL, "?") {
		t.Errorf("captured URL kept its query: %q", c.HTTPURL)
	}
	if !strings.Contains(c.ResponseBody, "assistant") {
		t.Errorf("the relayed response was not captured: %q", c.ResponseBody)
	}
	if c.ResponseHeaders["Request-Id"] != "req_up" {
		t.Errorf("response headers not captured: %v", c.ResponseHeaders)
	}
	// The whole evidence blob must not contain the raw credential anywhere.
	if strings.Contains(c.RequestHeaders["Authorization"]+c.RequestBody+c.ResponseBody, "sk-ant-fixture") {
		t.Error("the raw credential survived into the reported evidence")
	}
}

// TestUngatedCallMakesNoRoundTripThroughTheRelay is requirement 5, asserted through
// the real handler rather than only against Decide.
func TestUngatedCallMakesNoRoundTripThroughTheRelay(t *testing.T) {
	var got recorded
	upstream := upstreamRecorder(t, &got, nil)
	ev := &recordingEvaluator{verdict: "HALT"}
	// gated returns false for everything: nothing should be evaluated.
	gw := serveGateway(t, wire(t, upstream.URL, nil, ev, func(*http.Request) bool { return false }))

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(`{}`))
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("an ungated call was not forwarded: status %d", resp.StatusCode)
	}
	if ev.calls != 0 {
		t.Errorf("evaluator called %d times on an ungated call", ev.calls)
	}
}

// TestNilGatedPredicateGatesNothing pins the safe default. Where the predicate
// comes from is an open product decision, and until it is made the gateway must not
// invent one — a gateway that gated everything would add a round-trip to each of
// the ~52 model calls measured per turn window.
func TestNilGatedPredicateGatesNothing(t *testing.T) {
	var got recorded
	upstream := upstreamRecorder(t, &got, nil)
	ev := &recordingEvaluator{verdict: "HALT"}
	gw := serveGateway(t, wire(t, upstream.URL, nil, ev, nil))

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(`{}`))
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	if ev.calls != 0 {
		t.Errorf("a nil predicate still evaluated %d call(s)", ev.calls)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a nil predicate refused a call: status %d", resp.StatusCode)
	}
}

// TestGatedHaltRefusesAndNeverReachesUpstream is the enforcement path end to end.
func TestGatedHaltRefusesAndNeverReachesUpstream(t *testing.T) {
	var got recorded
	var reached bool
	upstream := upstreamRecorder(t, &got, func(w http.ResponseWriter) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	ev := &recordingEvaluator{verdict: "HALT", reason: "secrets policy"}
	em := &recordingEmitter{}
	gw := serveGateway(t, wire(t, upstream.URL, em, ev, func(*http.Request) bool { return true }))

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Authorization", fixtureCredential)
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != refusalStatus {
		t.Errorf("status = %d want the refusal status %d", resp.StatusCode, refusalStatus)
	}
	if reached {
		t.Error("the upstream was contacted for a refused call — refusal must stop it before it leaves")
	}
	if !strings.Contains(string(body), "refused by policy") {
		t.Errorf("the developer is not told why: %q", body)
	}
	if !strings.Contains(string(body), "secrets policy") {
		t.Errorf("the server's reason was dropped: %q", body)
	}
	// A refused call is exactly the one an auditor needs, so it must be reported.
	// A refused call is reported synchronously before the handler returns, but wait
	// anyway rather than depend on that ordering.
	seen := em.await(t, 1)
	if seen[0].CredentialFingerprint == "" {
		t.Error("the refusal's evidence carries no fingerprint")
	}
	if seen[0].RequestHeaders["Authorization"] != redactedHeaderValue {
		t.Error("the refusal's evidence carries an unredacted credential")
	}
}

// TestCaptureDoesNotBufferTheStream keeps the tee from reintroducing the failure
// phase 04 exists to prevent. The sink must not delay a byte.
func TestCaptureDoesNotBufferTheStream(t *testing.T) {
	const stall = 600 * time.Millisecond
	upstream := sseUpstream(t, func(w http.ResponseWriter, ctl *http.ResponseController) {
		io.WriteString(w, "event: message_start\ndata: {}\n\n")
		ctl.Flush()
		time.Sleep(stall)
		io.WriteString(w, "event: message_stop\ndata: {}\n\n")
		ctl.Flush()
	})
	em := &recordingEmitter{}
	gw := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(`{"stream":true}`))
	start := time.Now()
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	first := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("first byte: %v", err)
	}
	if ttfb := time.Since(start); ttfb >= stall/2 {
		t.Errorf("first byte took %v with a %v stall: the capture tee is buffering", ttfb, stall)
	}
	io.Copy(io.Discard, resp.Body)
}

// TestCaptureSinkIsBounded keeps a long stream from growing the buffer without
// limit, and keeps the collection bound LARGER than the wire cap so the cap still
// has something to truncate — the same relationship, and the same reason, as
// maxThinkingBytes to capBody.
func TestCaptureSinkIsBounded(t *testing.T) {
	if maxCaptureSinkBytes <= captureBodyRunes {
		t.Fatalf("collection bound %d must exceed the wire cap %d, or the cap is vacuous",
			maxCaptureSinkBytes, captureBodyRunes)
	}
	s := &captureSink{}
	chunk := strings.Repeat("x", 8192)
	for i := 0; i < (maxCaptureSinkBytes/len(chunk))+10; i++ {
		s.Write([]byte(chunk))
	}
	if len(s.String()) > maxCaptureSinkBytes {
		t.Errorf("sink grew to %d bytes, past its %d bound", len(s.String()), maxCaptureSinkBytes)
	}
}
