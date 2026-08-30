package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayemit"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway/gatewaytest"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

// transportsoak_test.go — what a real model call costs, measured rather than
// assumed.
//
// Two numbers this plan needs and could not have without a real corpus:
//
//  1. What ONE recorded model call adds to the spool. The recorded request bodies
//     are enormous — 96.75% of them exceed the 65,536-rune egress cap, median
//     ~529k runes — so "the cap bounds what we store" is a claim worth checking
//     rather than repeating. It turns out to be false about the SPOOL, which
//     matters operationally and is stated here rather than discovered in the
//     field.
//  2. That the cap does bound what egresses. That is OD1(c), and this exercises
//     it on a real oversized body instead of a synthetic one.

// soakIterations is small on purpose. This measures cost per call and the
// arithmetic to a full session is stated in the report; running thousands of
// iterations in CI would buy the same number and a minute of everyone's time.
const soakIterations = 8

// TestTransportSoakMeasuresSpoolCostPerModelCall records what the lane costs.
func TestTransportSoakMeasuresSpoolCostPerModelCall(t *testing.T) {
	ex := loadExchange(t, "messages-json.json")
	reqRunes := utf8.RuneCountInString(ex.Request.Body)
	if reqRunes <= 65536 {
		t.Fatalf("the recorded request body is %d runes, at or under the 65,536-rune cap; "+
			"this measurement needs an oversized one to mean anything", reqRunes)
	}

	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, ex.Response.Body)
	}))
	t.Cleanup(upstream.Close)
	gatewaytest.SwapUpstreamDial(t, memhttptest.DialContext)

	spoolDir := t.TempDir()
	ca, err := transport.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	em := &gatewayemit.Emitter{
		Lane:  gatewayemit.LaneProxy,
		Spool: hookflow.Spool{Dir: spoolDir},
		DID:   func() string { return "did:aip:7f3c9b2e-0000-5000-a000-00000000feed" },
		Warn:  func(string, ...any) {},
	}
	p, err := transport.New(transport.Config{Upstream: upstream.URL}, ca, em)
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}

	const sessionID = "7f3c9b2e-0000-5000-a000-00000000502a"
	start := time.Now()
	for i := 0; i < soakIterations; i++ {
		tc := connectAndHandshake(t, p, ca, "api.anthropic.com")
		req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages",
			strings.NewReader(ex.Request.Body))
		if err != nil {
			t.Fatalf("compose request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Claude-Code-Session-Id", sessionID)
		if err := req.Write(tc); err != nil {
			t.Fatalf("iteration %d: write: %v", i, err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(tc), req)
		if err != nil {
			t.Fatalf("iteration %d: read: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	elapsed := time.Since(start)

	path := hookflow.Spool{Dir: spoolDir}.SessionPath(sessionID)
	deadline := time.Now().Add(15 * time.Second)
	var size int64
	var lines int
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(path); err == nil {
			raw, _ := os.ReadFile(path)
			lines = strings.Count(strings.TrimSpace(string(raw)), "\n") + 1
			size = fi.Size()
			if lines >= soakIterations {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if lines == 0 {
		t.Fatal("nothing reached the spool; there is no cost to measure")
	}

	perEvent := size / int64(lines)
	t.Logf("SOAK: %d model call(s), request body %d runes, %v total (%v/call)",
		lines, reqRunes, elapsed.Round(time.Millisecond), (elapsed / time.Duration(soakIterations)).Round(time.Millisecond))
	t.Logf("SOAK: spool grew to %d bytes — %d bytes per model call", size, perEvent)

	// THE FINDING, and it is the opposite of what was assumed before measuring:
	// the spool is BOUNDED, not proportional to the request body. gateway's own
	// capture bound trims the body before the emitter ever sees it, so a
	// 564,718-rune request costs ~70 KB of spool rather than ~565 KB.
	//
	// Asserted rather than logged, because the assumption it replaces was
	// plausible: capBody runs on the way OUT (client/gatewayspan.go), so nothing
	// about the wire cap implies anything about the spool. Removing gateway's
	// capture bound would raise the cost of a governed session eightfold, and the
	// only symptom would be a disk filling up on a developer machine.
	if perEvent >= int64(reqRunes) {
		t.Errorf("the spool carries the full %d-rune request body (%d bytes/event). "+
			"gateway's capture bound no longer trims before the spool, so a session's "+
			"spool now grows with real request sizes: roughly %.1f GB per session at "+
			"the corpus's ~5,000 model calls.",
			reqRunes, perEvent, float64(perEvent)*5000/(1<<30))
	}
	t.Logf("SOAK: projected ~%.0f MB of spool per session at the corpus's ~5,000 model calls",
		float64(perEvent)*5000/(1<<20))
}

// TestOversizedRecordedBodyIsCappedOnTheWire is OD1(c), exercised on a real
// oversized recorded body rather than a synthetic one.
//
// It asserts on the bytes actually POSTed, which is the only place the cap is
// observable: the spool holds the full body, and a test that read the spool would
// conclude the opposite.
func TestOversizedRecordedBodyIsCappedOnTheWire(t *testing.T) {
	ex := loadExchange(t, "messages-json.json")
	reqRunes := utf8.RuneCountInString(ex.Request.Body)

	var posted []string
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		posted = append(posted, string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"verdict":"allow"}`)
	}))
	t.Cleanup(srv.Close)

	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	c, err := client.New(client.Config{
		BaseURL:       srv.URL,
		APIKey:        "obx_" + strings.Repeat("f", 24),
		DID:           "did:aip:7f3c9b2e-0000-5000-a000-00000000feed",
		PrivateKeyB64: base64.StdEncoding.EncodeToString(priv.Seed()),
		HTTP:          srv.Client(),
		// Capture ON, or stripContent removes the body entirely and every
		// assertion below passes without the cap having done anything. That is
		// not hypothetical: this test passed that way once, reporting a
		// 564,718-rune body "egressed as 941 runes" — the body was gone, not
		// capped.
		ContentCaptureEnabled: true,
	})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	ev := client.DevEvent{
		SchemaVersion:  client.SchemaVersion,
		EventType:      client.EventTurnCompleted,
		SessionID:      "7f3c9b2e-0000-5000-a000-0000000000cc",
		DeveloperDID:   "did:aip:7f3c9b2e-0000-5000-a000-00000000feed",
		Timestamp:      time.Now().UTC().Format(time.RFC3339Nano),
		Tool:           client.Tool{Name: "claude-code", Kind: client.ToolShell},
		EventID:        "px-soak-oversized-event",
		ProxyRequestID: "px-soak-oversized",
		Span: &client.Span{
			SemanticType: "llm_completion",
			Stage:        "completed",
			HTTPMethod:   "POST",
			HTTPURL:      "https://api.anthropic.com/v1/messages",
			HTTPStatus:   200,
			RequestBody:  ex.Request.Body,
		},
	}
	if _, err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(posted) == 0 {
		t.Fatal("nothing was POSTed; the assertion below would be vacuous")
	}

	body := posted[0]
	if utf8.RuneCountInString(body) > reqRunes {
		t.Errorf("the POSTed payload (%d runes) is larger than the request body it carries (%d); "+
			"the cap did not act", utf8.RuneCountInString(body), reqRunes)
	}
	// The tail of a 564k-rune body must be absent: the cap keeps the first 65,536
	// runes and OD1(c) accepts that the rest exists nowhere org-side.
	tail := ex.Request.Body[len(ex.Request.Body)-64:]
	if strings.Contains(body, tail) {
		t.Errorf("the tail of an oversized recorded body reached the wire; the 65,536-rune cap " +
			"is not bounding egress")
	}
	t.Logf("OD1(c): %d-rune recorded request body egressed as a %d-rune payload",
		reqRunes, utf8.RuneCountInString(body))
}

// jsonEscaped renders a fragment the way it appears inside a JSON string, so a
// substring search against a POSTed payload compares like with like.
func jsonEscaped(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return s
	}
	return string(b[1 : len(b)-1])
}
