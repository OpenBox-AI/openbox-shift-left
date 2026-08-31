package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"net/textproto"
	"sort"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
)

const fixtureCredential = "Bearer sk-ant-fixture-not-a-real-credential"

const fixtureAPIKey = "sk-ant-api03-fixture-not-a-real-credential"

const fixtureBody = `{"model":"claude-opus-4","system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI."},{"type":"text","text":"user context: café ☕ 日本語"}],"messages":[{"role":"user","content":"{\"nested\":\"pre-escaped\\\"quote\"}"}],"stream":true}`

type recorded struct {
	method    string
	target    string
	header    http.Header
	body      []byte
	length    int64
	transfer  []string
	hostValue string
}

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

func probeClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{DisableCompression: true, DialContext: memhttptest.DialContext},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func serveGateway(t *testing.T, g *Gateway) *memhttptest.Server {
	t.Helper()
	srv := memhttptest.NewServer(t, g)
	t.Cleanup(srv.Close)
	return srv
}

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
func TestForwardIdentity(t *testing.T) {
	var got recorded
	upstream := upstreamRecorder(t, &got, nil)
	gw := newTestGateway(t, upstream.URL)

	req, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages?beta=true", strings.NewReader(fixtureBody))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
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
	if got.target != "/v1/messages?beta=true" {
		t.Errorf("request target: got %q want %q", got.target, "/v1/messages?beta=true")
	}
	if string(got.body) != fixtureBody {
		t.Errorf("body not forwarded byte-identically:\n got %q\nwant %q", got.body, fixtureBody)
	}

	for k, v := range sent {
		if forwarded := got.header.Get(k); forwarded != v {
			t.Errorf("header %s: got %q want %q", k, forwarded, v)
		}
	}
	if forwarded := got.header.Get("Authorization"); forwarded != fixtureCredential {
		t.Errorf("Authorization did not pass through verbatim: got %q want %q", forwarded, fixtureCredential)
	}
	if forwarded := got.header.Get("X-Api-Key"); forwarded != fixtureAPIKey {
		t.Errorf("x-api-key did not pass through verbatim: got %q want %q", forwarded, fixtureAPIKey)
	}

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

// TestClientAcceptEncodingSurvives is the other half of the compression rule:
// an explicit client value must relay untouched rather than be replaced.
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

// TestSystemArrayIdentity checks the property a byte comparison cannot
// explain: that the system block is still a positional array with the
// attribution entry first.
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
// upstream 400's wording reaches the client as-is.
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
// static list cannot express.
func TestConnectionNamedHeadersDropped(t *testing.T) {
	var got recorded
	upstream := upstreamRecorder(t, &got, nil)
	gw := newTestGateway(t, upstream.URL)

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("X-Hop-Scoped", "should-not-survive")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Connection", "X-Hop-Scoped")

	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()

	if v := got.header.Get("X-Hop-Scoped"); v != "" {
		t.Errorf("a Connection-named header was forwarded: X-Hop-Scoped=%q", v)
	}
	if v := got.header.Get("Anthropic-Version"); v != "2023-06-01" {
		t.Errorf("an unrelated header was dropped: Anthropic-Version=%q", v)
	}
}

// TestOddRequestTargetsPassThrough pins the path half of byte-identity for
// targets that are valid but not what Go would have written itself. The relay
// forwards r.RequestURI anyway, because the raw request-target cannot be re-
// encoded by construction and needs no such argument to be correct.
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
