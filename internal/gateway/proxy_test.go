package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"net/textproto"
	"sort"
	"strings"
	"testing"
)

// fixtureCredential stands in for the developer's live credential. Pass-through
// means this exact string has to reach the upstream, so it is asserted by name.
const fixtureCredential = "Bearer sk-ant-fixture-not-a-real-credential"

// fixtureAPIKey is the other auth mode's carrier. Requirement 1 names both.
const fixtureAPIKey = "sk-ant-api03-fixture-not-a-real-credential"

// fixtureBody exercises the three body properties that a JSON round-trip would
// silently destroy: the positional `system` array (attribution block first),
// non-ASCII runes, and pre-escaped JSON inside a string.
const fixtureBody = `{"model":"claude-opus-4","system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI."},{"type":"text","text":"user context: café ☕ 日本語"}],"messages":[{"role":"user","content":"{\"nested\":\"pre-escaped\\\"quote\"}"}],"stream":true}`

// recorded is what the upstream actually received.
type recorded struct {
	method    string
	target    string
	header    http.Header
	body      []byte
	length    int64
	transfer  []string
	hostValue string
}

// upstreamRecorder serves as the provider and captures the forwarded request
// verbatim. respond lets a test choose the status and body it replies with.
func upstreamRecorder(t *testing.T, got *recorded, respond func(w http.ResponseWriter)) *memhttptest.Server {
	t.Helper()
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("upstream: reading body: %v", err)
		}
		got.method = r.Method
		got.target = r.URL.RequestURI()
		got.header = r.Header.Clone()
		got.body = body
		got.length = r.ContentLength
		got.transfer = append([]string(nil), r.TransferEncoding...)
		got.hostValue = r.Host
		if respond != nil {
			respond(w)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// probeClient talks to the gateway without adding anything of its own. Go's
// default client injects Accept-Encoding: gzip whenever a request carries none,
// and the gateway would then faithfully relay it -- which would make the
// no-additions assertion below untestable through any client.
func probeClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{DisableCompression: true, DialContext: memhttptest.DialContext},
		// Never follow a redirect either: these assertions are about what the
		// gateway returned, and a client that chased a 302 would report the
		// redirect target's answer as the gateway's.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// serveGateway exposes an already-constructed gateway over a real HTTP server, so
// the assertions run against bytes on a socket rather than a handler call. Tests
// that need to reach into the gateway first (the body-bound cases) construct it
// themselves and hand it here.
func serveGateway(t *testing.T, g *Gateway) *memhttptest.Server {
	t.Helper()
	srv := memhttptest.NewServer(t, g)
	t.Cleanup(srv.Close)
	return srv
}

// newTestGateway places a stock gateway in front of the given upstream.
func newTestGateway(t *testing.T, upstream string) *memhttptest.Server {
	t.Helper()
	g, err := New(Config{Addr: DefaultAddr, Upstream: upstream})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return serveGateway(t, g)
}

// TestForwardIdentity is the phase's load-bearing test: the forwarded request
// must carry the client's exact bytes onward.
//
// "Byte-identical" cannot be asserted literally at the header level — HTTP/2
// lowercases every field name on the wire — so identity is defined as
// set-equality of name->values against an enumerated exception set, asserted in
// BOTH directions. The no-additions direction is the one that matters: every
// default this relay has to defeat (X-Forwarded-For, an injected Accept-Encoding,
// a chunked-encoding flip) adds rather than removes.
func TestForwardIdentity(t *testing.T) {
	var got recorded
	upstream := upstreamRecorder(t, &got, nil)
	gw := newTestGateway(t, upstream.URL)

	req, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages?beta=true", strings.NewReader(fixtureBody))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	// Deliberately NO Accept-Encoding: its absence has to survive, because Go's
	// default Transport injects "gzip" and then transparently decompresses,
	// which would corrupt a relayed SSE stream.
	sent := map[string]string{
		"Authorization":            fixtureCredential,
		"X-Api-Key":                fixtureAPIKey,
		"Content-Type":             "application/json",
		"Anthropic-Version":        "2023-06-01",
		"Anthropic-Beta":           "some-unreleased-beta-2099-01-01",
		"Anthropic-Workspace-Id":   "wrk_fixture",
		"X-Claude-Code-Session-Id": "fixture-session",
	}
	for k, v := range sent {
		req.Header.Set(k, v)
	}

	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if got.method != http.MethodPost {
		t.Errorf("method: got %q want POST", got.method)
	}
	// The query string is part of the contract: requests arrive as
	// /v1/messages?beta=true and matching on path must not drop it.
	if got.target != "/v1/messages?beta=true" {
		t.Errorf("request target: got %q want %q", got.target, "/v1/messages?beta=true")
	}
	if string(got.body) != fixtureBody {
		t.Errorf("body not forwarded byte-identically:\n got %q\nwant %q", got.body, fixtureBody)
	}

	// Direction 1 -- no deletions, values exact.
	for k, v := range sent {
		if forwarded := got.header.Get(k); forwarded != v {
			t.Errorf("header %s: got %q want %q", k, forwarded, v)
		}
	}
	// Both credential headers named explicitly: pass-through auth is the invariant
	// this phase exists for, and the two auth modes use different carriers --
	// Authorization for OAuth, x-api-key for a console key. Whether OAuth even
	// follows ANTHROPIC_BASE_URL is still unresolved (ADR-0021 8), so the
	// x-api-key path may be the one that carries production traffic.
	if forwarded := got.header.Get("Authorization"); forwarded != fixtureCredential {
		t.Errorf("Authorization did not pass through verbatim: got %q want %q", forwarded, fixtureCredential)
	}
	if forwarded := got.header.Get("X-Api-Key"); forwarded != fixtureAPIKey {
		t.Errorf("x-api-key did not pass through verbatim: got %q want %q", forwarded, fixtureAPIKey)
	}

	// Direction 2 -- no additions. Anything the upstream saw that the client did
	// not send, minus the exceptions a relay is entitled to.
	allowed := map[string]bool{"Host": true, "Content-Length": true, "User-Agent": true}
	for name := range sent {
		allowed[textproto.CanonicalMIMEHeaderKey(name)] = true
	}
	var added []string
	for name := range got.header {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if allowed[canonical] || hopByHopHeaders[canonical] {
			continue
		}
		added = append(added, canonical)
	}
	sort.Strings(added)
	if len(added) > 0 {
		t.Errorf("relay added headers the client never sent: %v", added)
	}

	// Spelled out individually so a failure names the mutation, not just a diff.
	for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Via", "Accept-Encoding"} {
		if v := got.header.Get(name); v != "" {
			t.Errorf("relay added %s: %q (client sent none)", name, v)
		}
	}
	if got.length != int64(len(fixtureBody)) {
		t.Errorf("Content-Length: got %d want %d", got.length, len(fixtureBody))
	}
	if len(got.transfer) != 0 {
		t.Errorf("relay introduced Transfer-Encoding %v; a chunked flip changes the framing", got.transfer)
	}
}

// TestClientAcceptEncodingSurvives is the other half of the compression rule: an
// explicit client value must relay untouched rather than be replaced.
func TestClientAcceptEncodingSurvives(t *testing.T) {
	var got recorded
	upstream := upstreamRecorder(t, &got, nil)
	gw := newTestGateway(t, upstream.URL)

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()

	if v := got.header.Get("Accept-Encoding"); v != "identity" {
		t.Errorf("Accept-Encoding: got %q want %q", v, "identity")
	}
}

// TestSystemArrayIdentity checks the property a byte comparison cannot explain:
// that the system block is still a positional array with the attribution entry
// first. A future change that unmarshals and remarshals the body could keep the
// bytes valid while reordering it, which breaks the attribution strip and
// poisons the prompt-cache key.
func TestSystemArrayIdentity(t *testing.T) {
	var got recorded
	upstream := upstreamRecorder(t, &got, nil)
	gw := newTestGateway(t, upstream.URL)

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(fixtureBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()

	var decoded struct {
		System []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(got.body, &decoded); err != nil {
		t.Fatalf("forwarded body is not valid JSON: %v", err)
	}
	if len(decoded.System) != 2 {
		t.Fatalf("system: got %d entries want 2 (array-ness lost?)", len(decoded.System))
	}
	if !strings.Contains(decoded.System[0].Text, "Claude Code") {
		t.Errorf("attribution block is no longer first: got %q", decoded.System[0].Text)
	}
	if !strings.Contains(decoded.System[1].Text, "café ☕ 日本語") {
		t.Errorf("non-ASCII system entry corrupted: got %q", decoded.System[1].Text)
	}
}

// TestErrorBodyForwardedUnmodified keeps the gateway out of the error path: an
// upstream 400's wording reaches the client as-is. Claude Code matches on that
// wording, and an OpenBox envelope around it would read as an OpenBox bug.
func TestErrorBodyForwardedUnmodified(t *testing.T) {
	const upstreamError = `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: must be greater than 0"}}`
	var got recorded
	upstream := upstreamRecorder(t, &got, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_fixture")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, upstreamError)
	})
	gw := newTestGateway(t, upstream.URL)

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(`{}`))
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", resp.StatusCode)
	}
	if !bytes.Equal(body, []byte(upstreamError)) {
		t.Errorf("error body altered:\n got %q\nwant %q", body, upstreamError)
	}
	if v := resp.Header.Get("Request-Id"); v != "req_fixture" {
		t.Errorf("upstream response header dropped: Request-Id=%q", v)
	}
}

// TestConnectionNamedHeadersDropped covers the half of the hop-by-hop rule the
// static list cannot express. RFC 7230 6.1 makes any field NAMED INSIDE a
// Connection value connection-scoped for that message, so forwarding it would
// hand the upstream a directive describing only the local hop.
func TestConnectionNamedHeadersDropped(t *testing.T) {
	var got recorded
	upstream := upstreamRecorder(t, &got, nil)
	gw := newTestGateway(t, upstream.URL)

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("X-Hop-Scoped", "should-not-survive")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	// net/http writes Connection itself, so set it where the relay will read it.
	req.Header.Set("Connection", "X-Hop-Scoped")

	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()

	if v := got.header.Get("X-Hop-Scoped"); v != "" {
		t.Errorf("a Connection-named header was forwarded: X-Hop-Scoped=%q", v)
	}
	// The rule must not become an excuse to drop ordinary headers.
	if v := got.header.Get("Anthropic-Version"); v != "2023-06-01" {
		t.Errorf("an unrelated header was dropped: Anthropic-Version=%q", v)
	}
}

// TestOddRequestTargetsPassThrough pins the path half of byte-identity for
// targets that are valid but not what Go would have written itself.
//
// Honest scope, because it was measured rather than assumed: this does NOT
// discriminate between forwarding r.RequestURI and rebuilding via
// r.URL.RequestURI(). Both were compared across 12 adversarial targets (%2E, %41,
// %2f, a bare "?", "//", ";v=1", "+", UTF-8 escapes) and agreed on every one --
// url.EscapedPath returns RawPath verbatim whenever it is a valid encoding of
// Path, which it is for anything that parsed. The relay forwards r.RequestURI
// anyway, because the raw request-target cannot be re-encoded by construction
// and needs no such argument to be correct. What this test holds is the property
// itself: an unusual target reaches the upstream unchanged.
func TestOddRequestTargetsPassThrough(t *testing.T) {
	var got recorded
	upstream := upstreamRecorder(t, &got, nil)
	gw := newTestGateway(t, upstream.URL)

	const raw = "/v1/messages/%2E%2E?q=%41"
	req, err := http.NewRequest(http.MethodPost, gw.URL+raw, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()

	if got.target != raw {
		t.Errorf("request target changed in transit: got %q want %q", got.target, raw)
	}
}
