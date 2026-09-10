package gateway

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
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

// TestRequestAttributionOptionReceivesRawBytes is the seam this phase adds:
// WithRequestAttribution's closure runs on the RAW bytes read off the wire --
// byte-identical to what the client sent, before any selection or
// truncation -- and its result rides Captured.Attribution.
func TestRequestAttributionOptionReceivesRawBytes(t *testing.T) {
	upstream := upstreamRecorder(t, &recorded{}, nil)
	g, err := New(Config{Upstream: upstream.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	em := &recordingEmitter{}
	var calls int
	var gotRaw []byte
	g = g.WithCapture(em).WithRequestAttribution(func(raw []byte) map[string]string {
		calls++
		gotRaw = append([]byte(nil), raw...)
		return map[string]string{"prompt_id": "attr-1"}
	})
	srv := serveGateway(t, g)

	const body = `{"model":"claude-opus-4","messages":[]}`
	resp, err := probeClient().Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if calls != 1 {
		t.Fatalf("attribution closure called %d times, want 1", calls)
	}
	if string(gotRaw) != body {
		t.Errorf("attribution closure saw %q, want the raw body %q", gotRaw, body)
	}
	got := em.await(t, 1)
	if got[0].Attribution["prompt_id"] != "attr-1" {
		t.Errorf("Captured.Attribution[prompt_id] = %q, want it carried from the closure's result",
			got[0].Attribution["prompt_id"])
	}
}

// TestRequestAttributionNotCalledWithoutCapturing: with no emitter and no
// evaluator, nothing reads the evidence attribution would produce, so the
// closure must not run at all.
func TestRequestAttributionNotCalledWithoutCapturing(t *testing.T) {
	upstream := upstreamRecorder(t, &recorded{}, nil)
	g, err := New(Config{Upstream: upstream.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var calls int
	g = g.WithRequestAttribution(func([]byte) map[string]string {
		calls++
		return nil
	})
	srv := serveGateway(t, g)

	resp, err := probeClient().Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if calls != 0 {
		t.Errorf("attribution closure ran %d times with no emitter and no evaluator; "+
			"it must run only while capturing", calls)
	}
}

// TestRequestAttributionRunsIndependentOfKeepBodies: attribution is
// structural evidence and must not depend on whether this path keeps a body.
func TestRequestAttributionRunsIndependentOfKeepBodies(t *testing.T) {
	upstream := upstreamRecorder(t, &recorded{}, nil)
	g, err := New(Config{Upstream: upstream.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	em := &recordingEmitter{}
	var calls int
	g = g.WithCapture(em).
		WithBodyCapture(func(*http.Request) bool { return false }). // never keep a body
		WithRequestAttribution(func([]byte) map[string]string {
			calls++
			return map[string]string{"entrypoint": "cli"}
		})
	srv := serveGateway(t, g)

	resp, err := probeClient().Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if calls != 1 {
		t.Fatalf("attribution closure ran %d times, want 1 -- it must run even when "+
			"the path does not keep bodies", calls)
	}
	got := em.await(t, 1)
	if got[0].RequestBody != "" {
		t.Fatalf("fixture is not exercising keepBodies=false: RequestBody = %q", got[0].RequestBody)
	}
	if got[0].Attribution["entrypoint"] != "cli" {
		t.Errorf("Attribution did not ride Captured despite keepBodies=false: %v", got[0].Attribution)
	}
}

// --- Phase 04: response completeness (activity_output.openbox_capture) ---

// TestCaptureSinkCutIsTrueWhenMoreArrivedThanTheBufferKept is acceptance
// criterion 1, and the whole reason this phase does not length-compare.
// Write clamps buf to exactly maxCaptureSinkBytes, so len(Bytes()) can never
// exceed that number and capturableBody's own `len(body) > maxCaptureInputBytes`
// check can therefore never fire through the sink. seen counts every byte
// OFFERED, including what the clamp then dropped, so seen > len(Bytes()) can
// report the cut that the `>` check structurally cannot.
func TestCaptureSinkCutIsTrueWhenMoreArrivedThanTheBufferKept(t *testing.T) {
	sink := &captureSink{}
	sink.Write(bytes.Repeat([]byte("a"), maxCaptureSinkBytes+1))

	if got := len(sink.Bytes()); got != maxCaptureSinkBytes {
		t.Fatalf("fixture precondition: sink kept %d bytes, want exactly the bound %d", got, maxCaptureSinkBytes)
	}
	if got := sink.Seen(); got != maxCaptureSinkBytes+1 {
		t.Errorf("Seen() = %d, want %d (every byte offered, including the one the clamp dropped)",
			got, maxCaptureSinkBytes+1)
	}
	if !sink.Cut() {
		t.Error("Cut() = false at exactly the bound; len(Bytes()) can never exceed maxCaptureSinkBytes, " +
			"so a length comparison can never observe this and Cut() is the only thing that can")
	}
}

// TestCaptureSinkCutIsFalseWhenNothingWasDropped is the negative control: a
// body well under the bound must not be reported as cut.
func TestCaptureSinkCutIsFalseWhenNothingWasDropped(t *testing.T) {
	sink := &captureSink{}
	sink.Write([]byte("short reply"))

	if sink.Cut() {
		t.Error("Cut() = true for a response well under the bound")
	}
	if got, want := sink.Seen(), len("short reply"); got != want {
		t.Errorf("Seen() = %d, want %d", got, want)
	}
}

// TestCaptureSinkSeenAccumulatesAcrossManyWritesPastTheBound is acceptance
// criterion 6's arithmetic in isolation: streamTo offers the sink one
// relayBufferSize chunk at a time, so seen must keep counting every chunk
// offered, across every call, long after buf itself has stopped growing.
func TestCaptureSinkSeenAccumulatesAcrossManyWritesPastTheBound(t *testing.T) {
	sink := &captureSink{}
	const chunk = relayBufferSize
	chunks := maxCaptureSinkBytes/chunk + 3 // past the bound by 3 whole chunks
	for range chunks {
		sink.Write(bytes.Repeat([]byte("x"), chunk))
	}

	if got, want := sink.Seen(), chunks*chunk; got != want {
		t.Errorf("Seen() = %d, want %d (every chunk offered, across every Write call)", got, want)
	}
	if got := len(sink.Bytes()); got != maxCaptureSinkBytes {
		t.Errorf("Bytes() length = %d, want exactly the bound %d", got, maxCaptureSinkBytes)
	}
	if !sink.Cut() {
		t.Error("Cut() = false despite seen exceeding the kept buffer by three whole chunks")
	}
}

// TestCaptureSinkNilIsSafe: an emitter-less relay never allocates a sink, and
// every accessor has to tolerate that nil rather than the caller branching on
// it everywhere sink is read.
func TestCaptureSinkNilIsSafe(t *testing.T) {
	var sink *captureSink
	if sink.Cut() {
		t.Error("Cut() on a nil sink must be false")
	}
	if sink.Seen() != 0 {
		t.Error("Seen() on a nil sink must be 0")
	}
	if sink.Bytes() != nil {
		t.Error("Bytes() on a nil sink must be nil")
	}
	sink.Write([]byte("must not panic")) // no-op on a nil receiver
}

// TestResponseTruncatedIsTrueWhenStoredEndsInBodyCutNoteEvenIfTheSinkDidNot is
// acceptance criterion 3: capRunes and decodeCapturable can each append
// bodyCutNote to the STORED form without the sink itself ever reaching its own
// bound (a small compressed body decoding to a huge plaintext, or redaction
// growth pushing an already-under-bound body over captureBodyRunes). The OR
// has to read the stored string, not stop at asking the sink.
func TestResponseTruncatedIsTrueWhenStoredEndsInBodyCutNoteEvenIfTheSinkDidNot(t *testing.T) {
	sink := &captureSink{}
	sink.Write([]byte("short"))
	if sink.Cut() {
		t.Fatal("fixture precondition: the sink must not have cut anything")
	}

	stored := "whatever survived the cut" + bodyCutNote
	if !responseTruncated(sink, stored, nil) {
		t.Error("responseTruncated = false for a stored body ending in bodyCutNote")
	}
}

// TestResponseTruncatedIsFalseForAWholeShortBody is the negative control for
// the OR above: neither input observed a loss, so the report must not claim
// one either.
func TestResponseTruncatedIsFalseForAWholeShortBody(t *testing.T) {
	sink := &captureSink{}
	sink.Write([]byte("{}"))
	if responseTruncated(sink, "{}", nil) {
		t.Error("responseTruncated = true for an untruncated body under every bound")
	}
}

// TestShortResponseReportsCompleteWithExactByteCount is acceptance criterion 2,
// end to end: a short reply must carry truncated:false and original_bytes
// equal to its own length, not merely leave the sink able to say so.
func TestShortResponseReportsCompleteWithExactByteCount(t *testing.T) {
	const body = `{"type":"message","role":"assistant","content":[{"type":"text","text":"hi"}]}`
	upstream := upstreamRecorder(t, &recorded{}, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, body)
	})
	em := &recordingEmitter{}
	g, err := New(Config{Upstream: upstream.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g = g.WithCapture(em)
	srv := serveGateway(t, g)

	resp, err := probeClient().Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got := em.await(t, 1)[0]
	if got.ResponseTruncated {
		t.Error("ResponseTruncated = true for a short, whole response")
	}
	if got.ResponseBytesSeen != len(body) {
		t.Errorf("ResponseBytesSeen = %d, want %d", got.ResponseBytesSeen, len(body))
	}
}

// TestResponseOverTheSinkBoundReportsTruncatedWithFullByteCountSeen is
// acceptance criteria 1 and 6, wired end to end: a 300 KB identity-encoded
// response is exactly the shape whose sink-kept length lands on the bound
// (invisible to a `>` check) while original_bytes must still equal the FULL
// pre-cut count the relay actually read off the wire, which requires seen to
// increment before the sink's own bound check and clamp.
func TestResponseOverTheSinkBoundReportsTruncatedWithFullByteCountSeen(t *testing.T) {
	const fullSize = 300 * 1024
	body := strings.Repeat("r", fullSize)
	if fullSize <= maxCaptureSinkBytes {
		t.Fatalf("fixture precondition: %d must exceed the sink bound %d", fullSize, maxCaptureSinkBytes)
	}
	upstream := upstreamRecorder(t, &recorded{}, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, body)
	})
	em := &recordingEmitter{}
	g, err := New(Config{Upstream: upstream.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g = g.WithCapture(em)
	srv := serveGateway(t, g)

	resp, err := probeClient().Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got := em.await(t, 1)[0]
	if got.ResponseBytesSeen != fullSize {
		t.Errorf("ResponseBytesSeen = %d, want the full %d bytes offered, not the stored length",
			got.ResponseBytesSeen, fullSize)
	}
	if !got.ResponseTruncated {
		t.Error("ResponseTruncated = false for a response well over the sink's bound")
	}
}

// TestCompressedResponseOverItsOwnBoundIsTruncatedEvenThoughTheSinkNeverFilled
// is acceptance criterion 3's realistic case: a small compressed body that
// decodes to a huge plaintext never fills the sink (the sink only ever sees
// the COMPRESSED bytes), yet decodeCapturable's own bound cuts the decoded
// text and marks it -- so the report must still say truncated:true.
func TestCompressedResponseOverItsOwnBoundIsTruncatedEvenThoughTheSinkNeverFilled(t *testing.T) {
	plain := strings.Repeat("z", maxCaptureInputBytes+1024)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte(plain)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if gz.Len() >= maxCaptureSinkBytes {
		t.Fatalf("fixture does not exercise the gap: compressed size %d is not below the sink bound %d",
			gz.Len(), maxCaptureSinkBytes)
	}

	upstream := upstreamRecorder(t, &recorded{}, func(w http.ResponseWriter) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		w.Write(gz.Bytes())
	})
	em := &recordingEmitter{}
	g, err := New(Config{Upstream: upstream.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g = g.WithCapture(em)
	srv := serveGateway(t, g)

	resp, err := probeClient().Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got := em.await(t, 1)[0]
	if !strings.HasSuffix(got.ResponseBody, bodyCutNote) {
		t.Fatalf("fixture precondition: stored body does not end in bodyCutNote (%d bytes stored)",
			len(got.ResponseBody))
	}
	if !got.ResponseTruncated {
		t.Error("ResponseTruncated = false for a body the compressed-decode path itself cut, even " +
			"though the sink's own bound (measured on the COMPRESSED bytes) never fired")
	}
}

// --- Two more abort paths responseTruncated must catch, neither of which is
// the sink hitting its own bound or a downstream cut-note: the connection can
// break on either SIDE of the relay, and each side leaves a different trace.

// TestResponseTruncatedIsTrueWhenTheUpstreamConnectionBreaksMidStream: the
// connection to the UPSTREAM breaks mid-body, so streamTo's own read fails
// (streamErr != nil) before EOF. The body captured so far is short, so
// neither the sink's bound nor a bodyCutNote suffix would ever have caught
// this on their own -- streamErr itself has to be consulted.
func TestResponseTruncatedIsTrueWhenTheUpstreamConnectionBreaksMidStream(t *testing.T) {
	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\n\n"))
		http.NewResponseController(w).Flush()
		panic(http.ErrAbortHandler) // kill the connection mid-body
	}))
	t.Cleanup(upstream.Close)

	em := &recordingEmitter{}
	srv := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

	resp, err := probeClient().Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	got := em.await(t, 1)[0]
	if !got.ResponseTruncated {
		t.Error("ResponseTruncated = false for a response the connection to the UPSTREAM broke mid-body " +
			"(streamTo's own read error was never consulted)")
	}
}

// abortingResponseWriter fails Write after failAfter successful calls,
// modelling the developer's own tool disconnecting mid-stream: the relay's
// write TO THE CLIENT fails, which is a different fact than an upstream read
// error and arrives through a different return path (see streamTo).
type abortingResponseWriter struct {
	header    http.Header
	failAfter int
	writes    int
}

func (w *abortingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *abortingResponseWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > w.failAfter {
		return 0, errors.New("simulated broken pipe: the client disconnected mid-stream")
	}
	return len(p), nil
}

func (w *abortingResponseWriter) WriteHeader(int) {}

// TestResponseTruncatedIsTrueWhenTheClientDisconnectsMidStream is the other
// abort path: the developer's own tool hangs up mid-generation (Ctrl-C), so
// streamTo's write TO THE CLIENT fails instead of its read from upstream.
// streamTo returns nil for this deliberately -- relaying is done trying, and
// turning it into an error here would incorrectly trip the relay's own
// panic(http.ErrAbortHandler) path -- so streamErr is nil and cannot be what
// reports this cut.
func TestResponseTruncatedIsTrueWhenTheClientDisconnectsMidStream(t *testing.T) {
	sink := &captureSink{}
	// Two full relayBufferSize chunks: the first write succeeds and is
	// captured, the second fails -- proving the cut is detected after at
	// least one chunk was already relayed, not only at stream start.
	src := strings.NewReader(strings.Repeat("a", relayBufferSize) + strings.Repeat("b", relayBufferSize))
	w := &abortingResponseWriter{failAfter: 1}
	g := &Gateway{}

	streamErr := g.streamTo(w, src, sink)
	if streamErr != nil {
		t.Fatalf("streamTo returned %v, want nil: a downstream write failure must not surface as a "+
			"stream error, or it would flip the relay's own panic path", streamErr)
	}
	if got := len(sink.Bytes()); got != relayBufferSize {
		t.Fatalf("fixture precondition: sink kept %d bytes, want exactly one chunk (%d) -- the "+
			"second chunk's write failed before it could ever reach the sink", got, relayBufferSize)
	}
	if !sink.Incomplete() {
		t.Error("sink.Incomplete() = false after a downstream write failed mid-stream; streamTo's " +
			"write-failure branch must mark it, since seen and len(Bytes()) agree exactly here and " +
			"Cut() cannot see this case")
	}

	if !responseTruncated(sink, sink.String(), streamErr) {
		t.Error("responseTruncated = false for a stream that stopped because the client disconnected; " +
			"streamErr is nil here by design, so sink.Incomplete() must be what reports this cut")
	}
}
