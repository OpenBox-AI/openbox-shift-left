// Package fakecore is an in-process stand-in for the OpenBox control plane,
// for tests that grade what the binary actually put on the wire.
//
// It is deliberately not a mock that answers a canned 200. It verifies the AIP
// signature against its own reimplementation of the canonical string -- a
// verifier that shares the signer's code proves nothing -- records every
// accepted request verbatim, and answers a verdict the test scripted per tool
// call.
//
// It never dedupes. Two requests carrying the same Idempotency-Key are two
// entries in the inbox, because collapsing them would hide exactly the
// double-delivery class the gate/observe election exists to prevent.
//
// Test-only; TestFakecoreStaysTestOnly is the tripwire.
package fakecore

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
)

// The routes core serves. Spelled out rather than imported from the client:
// the fake stands in for the far end, so it owns its own copy of the contract
// it is emulating. A shared constant could be renamed on both sides at once
// and the test would still pass.
const (
	evaluatePath = "/api/v1/governance/evaluate"
	approvalPath = "/api/v1/governance/approval"
)

// Header names core reads. Same rationale as the paths above.
const (
	hdrAgentDID   = "X-OpenBox-Agent-DID"
	hdrAgentTS    = "X-OpenBox-Agent-Timestamp"
	hdrAgentNonce = "X-OpenBox-Agent-Nonce"
	hdrAgentSig   = "X-OpenBox-Agent-Signature"
	hdrBodySHA    = "X-OpenBox-Body-SHA256"
)

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
	t      TB
	srv    *memhttptest.Server
	script Script

	pub     ed25519.PublicKey
	seedB64 string
	did     string

	mu            sync.Mutex
	inbox         []Received
	received      int
	scripted      int
	outage        bool
	hits          int
	rejected      []string
	approval      func(Received) (int, string)
	approvalPolls int
}

// New starts a fake core and mints the keypair the client under test must sign
// with. The seed is generated per server, never committed: a fixture seed
// shared with the signer would let a broken signer pass.
func New(t TB, s Script) *Server {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	if s.SeedB64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(s.SeedB64)
		if err != nil || len(decoded) != ed25519.SeedSize {
			t.Fatalf("fakecore: Script.SeedB64 is not a %d-byte base64 seed", ed25519.SeedSize)
		}
		seed = decoded
	} else if _, err := rand.Read(seed); err != nil {
		t.Fatalf("fakecore: generate seed: %v", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	f := &Server{
		t:       t,
		script:  s.withDefaults(),
		pub:     priv.Public().(ed25519.PublicKey),
		seedB64: base64.StdEncoding.EncodeToString(seed),
		did:     "did:aip:00000000-0000-4000-8000-00000000f00d",
	}
	f.srv = memhttptest.NewServer(t, http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// URL is the base URL to point OPENBOX_BASE_URL at.
func (f *Server) URL() string { return f.srv.URL }

// SeedB64 is the private key the client must sign with to be accepted.
func (f *Server) SeedB64() string { return f.seedB64 }

// DID is the agent identity this server expects.
func (f *Server) DID() string { return f.did }

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

	raw, _ := io.ReadAll(r.Body)

	switch r.URL.Path {
	case evaluatePath:
		f.serveEvaluate(w, r, raw)
	case approvalPath:
		f.serveApproval(w, r, raw)
	default:
		// Recorded and answered, never left to hang: a route the fake does not
		// know is a defect in the test or a new client route, and either way the
		// caller must not block on it.
		f.reject(w, http.StatusNotFound, "unknown path "+r.URL.Path)
	}
}

func (f *Server) serveEvaluate(w http.ResponseWriter, r *http.Request, raw []byte) {
	if reason, ok := f.verify(r, http.MethodPost, evaluatePath, raw); !ok {
		f.reject(w, http.StatusUnauthorized, reason)
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		f.reject(w, http.StatusBadRequest, "body is not a JSON object")
		return
	}
	rec := Received{Headers: r.Header.Clone(), Raw: raw, Body: body}
	// The wire contract is checked at the door, so a malformed body is a
	// refusal rather than a row a later grader has to notice. Core would
	// refuse it too.
	if reasons := checkWireShape(body); len(reasons) > 0 {
		f.reject(w, http.StatusBadRequest, "wire shape: "+strings.Join(reasons, "; "))
		return
	}

	f.mu.Lock()
	if f.outage {
		f.scripted++
		f.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	f.received++
	// The nth call is counted on arrival, not on acceptance: a test saying
	// "the second call fails" means the second request, and numbering by
	// acceptances would renumber itself as soon as one of them did.
	status, verdict := f.script.answer(f.received, rec.ToolUseID())
	if status >= 200 && status < 300 {
		// Only an accepted request is in the inbox. A refused attempt was not
		// delivered, and counting it would make a redelivery after an outage
		// look like a double delivery -- which is the property the outage
		// scenario exists to check.
		f.inbox = append(f.inbox, rec)
	} else {
		// A scripted failure is the test's own doing and is counted apart from
		// a refusal the fake decided on: a caller asserting "nothing I sent
		// was malformed" must not be answered by an outage it asked for.
		f.scripted++
	}
	delay := f.script.Delay
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, verdict)
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

func (f *Server) serveApproval(w http.ResponseWriter, r *http.Request, raw []byte) {
	if reason, ok := f.verify(r, r.Method, approvalPath, raw); !ok {
		f.reject(w, http.StatusUnauthorized, reason)
		return
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)

	f.mu.Lock()
	f.approvalPolls++
	fn := f.approval
	f.mu.Unlock()

	if fn == nil {
		f.reject(w, http.StatusNotFound, "approval polled but no handler is installed")
		return
	}
	status, resp := fn(Received{Headers: r.Header.Clone(), Raw: raw, Body: body})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, resp)
}

// verify reimplements the AIP check core performs. Deliberately a second
// implementation: a verifier calling the signer's own canonicalString would
// agree with it by construction, including when both are wrong.
func (f *Server) verify(r *http.Request, method, path string, body []byte) (string, bool) {
	sum := sha256.Sum256(body)
	wantSHA := hex.EncodeToString(sum[:])
	if got := r.Header.Get(hdrBodySHA); got != wantSHA {
		return "body digest header does not match the body", false
	}
	if got := r.Header.Get(hdrAgentDID); !strings.HasPrefix(got, "did:aip:") {
		return "agent DID header is absent or not a did:aip", false
	}
	canonical := strings.ToUpper(method) + "\n" + path + "\n" +
		r.Header.Get(hdrAgentTS) + "\n" +
		r.Header.Get(hdrAgentNonce) + "\n" + wantSHA
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get(hdrAgentSig))
	if err != nil {
		return "signature header is not base64", false
	}
	if !ed25519.Verify(f.pub, []byte(canonical), sig) {
		return "signature does not verify against the expected key", false
	}
	return "", true
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
