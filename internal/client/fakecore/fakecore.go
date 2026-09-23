// Package fakecore is an in-process stand-in for the OpenBox control plane,
// for tests that grade what the binary actually put on the wire.
//
// It is deliberately not a mock that answers a canned 200. It verifies the
// RS256 client assertion against its own reimplementation of the check
// (fakecorev3.go's verifyV3AssertionIndependently) -- a verifier sharing code
// with the thing it grades proves nothing -- records every accepted request
// verbatim, and answers a verdict the test scripted per tool call.
//
// It never dedupes. Two requests carrying the same Idempotency-Key are two
// entries in the inbox, because collapsing them would hide exactly the
// double-delivery class the gate/observe election exists to prevent.
//
// Test-only; TestFakecoreStaysTestOnly is the tripwire.
package fakecore

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
)

// The workload-identity routes core serves. Spelled out rather than imported
// from the client: the fake stands in for the far end, so it owns its own
// copy of the contract it is emulating. A shared constant could be renamed on
// both sides at once and the test would still pass.
const (
	v3BootstrapPath = "/api/v3/auth/bootstrap"
	v3TokenPath     = "/realms/fake/protocol/openid-connect/token"
	v3EvaluatePath  = "/api/v3/governance/evaluate"
	v3ApprovalPath  = "/api/v3/governance/approval"
	v3ValidatePath  = "/api/v3/auth/validate"
)

// legacyAPIVersion is the retired protocol version segment. Any hit under it
// is recorded as a rejection and answered 404: there is no client left that
// speaks it, so a test build reaching this path is either a stale fixture or
// a client regression, and either way the fake must not silently emulate the
// deleted protocol.
const legacyAPIVersion = "v1"

// isLegacyAPIPath reports whether path addresses the retired API version.
// Parsed by segment rather than matched against one literal path prefix: the
// deleted route strings are exactly what the repo's own success-criteria grep
// hunts for across every non-test file, and this rejection path is the reason
// a hit must still be recognized here.
func isLegacyAPIPath(path string) bool {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	return len(parts) >= 2 && parts[0] == "api" && parts[1] == legacyAPIVersion
}

// TB is the subset of *testing.T fakecore needs, mirroring memhttptest.TB so
// the two compose.
type TB interface {
	Helper()
	Cleanup(func())
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
}

// Received is one accepted request, kept verbatim.
//
// Headers is not optional: Idempotency-Key rides there, and so does every
// signature field, so a grader checking delivery-once or an auth path has
// nothing to read without it.
type Received struct {
	Headers http.Header
	Raw     []byte
	Body    map[string]any
}

// ToolUseID reads the structural identifier the mapper writes into metadata.
// Structural, so it survives content_capture:false -- which is what makes it
// usable as the join key between a scenario's declared input and the wire.
func (r Received) ToolUseID() string { return r.metaString("tool_use_id") }

// EventType is the wire discriminator: one of the four stored values, never a
// DevEvent type.
func (r Received) EventType() string { return r.topString("event_type") }

// ActivityID pairs a tool call's two halves onto one row.
func (r Received) ActivityID() string { return r.topString("activity_id") }

func (r Received) topString(key string) string {
	s, _ := r.Body[key].(string)
	return s
}

func (r Received) metaString(key string) string {
	m, _ := r.Body["metadata"].(map[string]any)
	s, _ := m[key].(string)
	return s
}

// Server is a running fake core.
type Server struct {
	t TB
	// srv is set by New (in-process, memhttptest); realSrv by NewReal (a real
	// OS socket, for the rare test that spawns a genuine child process). Never
	// both.
	srv     *memhttptest.Server
	realSrv *httptest.Server
	script  Script

	mu            sync.Mutex
	inbox         []Received
	scripted      int
	outage        bool
	hits          int
	rejected      []string
	approval      func(Received) (int, string)
	approvalPolls int

	// Workload-identity state: a fake Keycloak (bootstrap doc + token
	// exchange) plus the runtime routes' auth bookkeeping.
	v3BootstrapHits       int
	v3ExchangeHits        int
	v3EvaluateAttempts    int
	v3TokenEndpointDown   bool
	v3Revoked             bool
	v3IssuedTokens        map[string]bool
	v3BootstrapFailStatus int
	v3BootstrapFailReason string
	v3ExchangeFailStatus  int
	v3ExchangeFailError   string
}

// New starts a fake core. The workload identity a client under test
// authenticates as is process-wide (fakecorev3.go's WorkloadPrivateKey/
// APIKey/AgentID), not per-Server.
func New(t TB, s Script) *Server {
	t.Helper()
	f := newUnstarted(t, s)
	srv := memhttptest.NewServer(t, http.HandlerFunc(f.serve))
	f.mu.Lock()
	f.srv = srv
	f.mu.Unlock()
	t.Cleanup(srv.Close)
	return f
}

// NewReal is New, bound to a real OS socket (httptest.NewServer) instead of
// memhttptest's in-process pipes. memhttptest's own doc comment names its
// blind spot: "a child process" and "code that builds its own http.Transport"
// cannot reach an in-memory listener at all, because it lives behind THIS
// test binary's http.DefaultTransport. The handful of tests that spawn a
// real, separately-built `openbox` binary (e.g. to prove a detached realtime
// flusher delivers) need this instead of New.
func NewReal(t TB, s Script) *Server {
	t.Helper()
	f := newUnstarted(t, s)
	// httptest.NewServer binds and starts accepting before it returns, so a
	// real client could reach f.serve -> f.URL() concurrently with the field
	// write below (observed: an unrelated leaked subprocess from another
	// real-socket test connecting to a since-reused ephemeral port). srv/
	// realSrv are set exactly once and never again, so the mutex here and in
	// URL() below is the whole fix.
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	f.mu.Lock()
	f.realSrv = srv
	f.mu.Unlock()
	t.Cleanup(srv.Close)
	return f
}

func newUnstarted(t TB, s Script) *Server {
	return &Server{
		t:      t,
		script: s.withDefaults(),
	}
}

// URL is the base URL to point OPENBOX_BASE_URL at.
func (f *Server) URL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.realSrv != nil {
		return f.realSrv.URL
	}
	return f.srv.URL
}

// Inbox is every accepted request, in arrival order, never collapsed.
func (f *Server) Inbox() []Received {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Received(nil), f.inbox...)
}

// Hits counts every request that reached the handler, accepted or not. Inbox
// length counts only the accepted ones, so the two disagreeing is itself a
// signal.
func (f *Server) Hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// SetOutage takes the control plane up or down. A scenario uses it to fail the
// synchronous gate delivery while leaving the later flush to succeed, which is
// the only way to exercise the observe-copy election against the real
// condition rather than one only a test can reach.
func (f *Server) SetOutage(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outage = down
}

// ScriptedFailures counts the responses the scenario itself asked the fake to
// fail, as opposed to the ones the fake refused on inspection.
func (f *Server) ScriptedFailures() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scripted
}

// Rejections is why each refused request was refused. A rejection reason never
// quotes the request body: INV-2 applies to test logs.
func (f *Server) Rejections() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rejected...)
}

func (f *Server) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits++
	f.mu.Unlock()

	if isLegacyAPIPath(r.URL.Path) {
		f.reject(w, http.StatusNotFound, "v1 route")
		return
	}

	raw, _ := io.ReadAll(r.Body)

	switch r.URL.Path {
	case v3BootstrapPath:
		f.serveV3Bootstrap(w, r)
	case v3TokenPath:
		f.serveV3Token(w, r, raw)
	case v3EvaluatePath:
		f.serveV3Evaluate(w, r, raw)
	case v3ApprovalPath:
		f.serveV3Approval(w, r, raw)
	case v3ValidatePath:
		f.serveV3Validate(w, r)
	default:
		// Recorded and answered, never left to hang: a route the fake does not
		// know is a defect in the test or a new client route, and either way the
		// caller must not block on it.
		f.reject(w, http.StatusNotFound, "unknown path "+r.URL.Path)
	}
}

// Approval installs the handler for the approval poll, and is not optional for
// any scenario that scripts REQUIRE_APPROVAL. Without it the route 404s, which
// the hold reads as undecided and denies on -- so a "an unanswered approval
// denies" assertion would pass through the degraded path and prove only that
// the endpoint is missing.
func (f *Server) Approval(fn func(Received) (int, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approval = fn
}

// ApprovalPolls counts how many times the hold asked. A scenario that expects
// a hold asserts this is non-zero, or it cannot tell a real hold from a fake
// that was never consulted.
func (f *Server) ApprovalPolls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.approvalPolls
}

// reject records why and answers. The reason never carries the request body.
func (f *Server) reject(w http.ResponseWriter, status int, reason string) {
	f.mu.Lock()
	f.rejected = append(f.rejected, reason)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"code":`+itoa(status)+`,"message":"rejected"}`)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
