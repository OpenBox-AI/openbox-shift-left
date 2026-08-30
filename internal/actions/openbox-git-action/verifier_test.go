package gitaction

import (
	"context"
	"net/http"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testAgentID is a well-formed agent UUID; testPusherDID is the DID it derives to
// (computed via didForAgent so the construction bind passes). This models what CI
// supplies: a matching (OPENBOX_AGENT_ID, OPENBOX_DID) pair.
const testAgentID = "11111111-1111-1111-1111-111111111111"

func testPusherDID(t *testing.T) string {
	t.Helper()
	did, err := DIDForAgent(testAgentID)
	if err != nil {
		t.Fatalf("didForAgent: %v", err)
	}
	return did
}

// mockBackend is a mock openbox-backend session-read endpoint. It records the
// last request (to assert the path, query, and X-API-Key), and serves a
// canned status/body for GET /agent/<agentID>/sessions.
type mockBackend struct {
	srv     *memhttptest.Server
	calls   int32
	lastReq *http.Request

	status int
	body   string
	delay  time.Duration
}

func newMockBackend(t *testing.T, status int, body string) *mockBackend {
	t.Helper()
	m := &mockBackend{status: status, body: body}
	m.srv = memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&m.calls, 1)
		m.lastReq = r.Clone(context.Background())
		if m.delay > 0 {
			time.Sleep(m.delay)
		}
		w.WriteHeader(m.status)
		_, _ = w.Write([]byte(m.body))
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// verifier builds a real apiVerifier pointed at the mock (loopback http is
// allowed). timeout is optional (0 → default).
func (m *mockBackend) verifier(t *testing.T, timeout time.Duration) OwnershipVerifier {
	t.Helper()
	v, err := NewAPIVerifier(APIVerifierConfig{
		BaseURL:   m.srv.URL,
		AgentID:   testAgentID,
		PusherDID: testPusherDID(t),
		OrgAPIKey: "obx_key_test",
		Timeout:   timeout,
	})
	if err != nil {
		t.Fatalf("NewAPIVerifier: %v", err)
	}
	return v
}

// sessionsBody builds a body matching the REAL openbox-backend wire shape (verified
// live 2026-07-13): a global envelope {status, data:<payload>} wrapping the
// SessionListResponseDto {data:[…]} — so rows are at data.data[].
func sessionsBody(runIDs ...string) string {
	return sessionsBodyForAgent(testAgentID, runIDs...)
}

func sessionsBodyForAgent(agentID string, runIDs ...string) string {
	var b strings.Builder
	b.WriteString(`{"status":200,"data":{"data":[`)
	for i, id := range runIDs {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"run_id":"` + id + `","agent_id":"` + agentID + `","status":"completed"}`)
	}
	b.WriteString(`]}}`)
	return b.String()
}

// --- UUIDv5 correctness (the INV-4 bind rests on this) ----------------------

func TestUUIDV5_MatchesReferenceVector(t *testing.T) {
	// RFC-4122 / canonical `uuid` library vector: v5(NAMESPACE_DNS, "python.org").
	// If this drifts, didForAgent would reject every real (agentID,DID) pair.
	const nsDNS = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	got, err := uuidV5(nsDNS, "python.org")
	if err != nil {
		t.Fatal(err)
	}
	const want = "886313e1-3b8a-5372-9b90-0c9aee199e5d"
	if got != want {
		t.Fatalf("uuidV5 = %s, want %s (implementation does not match the uuid lib)", got, want)
	}
}

// --- OwnsSession contract ---------------------------------------------------

func TestAPIVerifier_OwnedSessionIsOwned(t *testing.T) {
	m := newMockBackend(t, 200, sessionsBody("sess-A"))
	v := m.verifier(t, 0)

	if ok, err := v.OwnsSession(ctx, "sess-A"); !ok || err != nil {
		t.Fatalf("OwnsSession(sess-A) = (%v, %v), want (true, nil)", ok, err)
	}
}

func TestAPIVerifier_NotOwnedWhenNoRow(t *testing.T) {
	m := newMockBackend(t, 200, `{"status":200,"data":{"data":[]}}`)
	v := m.verifier(t, 0)

	if ok, err := v.OwnsSession(ctx, "sess-A"); ok || err != nil {
		t.Fatalf("empty data = (%v,%v), want (false,nil)", ok, err)
	}
}

func TestAPIVerifier_MatchesRunIDNotSessionEntityID(t *testing.T) {
	// The trailer value must match run_id, NOT the SessionEntity `id` PK.
	m := newMockBackend(t, 200, `{"status":200,"data":{"data":[{"id":"sess-A","run_id":"other-run","agent_id":"`+testAgentID+`"}]}}`)
	v := m.verifier(t, 0)

	if ok, _ := v.OwnsSession(ctx, "sess-A"); ok {
		t.Fatal("matched on id PK; must match run_id only (SL-15 correctness detail)")
	}
}

func TestAPIVerifier_ParsesRealBackendEnvelope(t *testing.T) {
	// Regression pin for the live finding (2026-07-13): the backend double-nests the
	// rows under data.data[] (global {status,data} envelope wrapping the DTO). A body
	// captured verbatim from the running backend must resolve as owned.
	body := `{"status":200,"data":{"data":[{"id":"941a5940-b69e-4930-941e-79ae0d5bf943",` +
		`"agent_id":"` + testAgentID + `","workflow_id":"` + testPusherDID(t) + `",` +
		`"run_id":"606a02c8-c982-415c-823e-8887b3e8b8b7","status":"completed",` +
		`"metadata":null,"current_step":{"event_type":"SessionStarted"}}],"total":1,"page":1}}`
	m := newMockBackend(t, 200, body)
	if ok, err := m.verifier(t, 0).OwnsSession(ctx, "606a02c8-c982-415c-823e-8887b3e8b8b7"); !ok || err != nil {
		t.Fatalf("real backend envelope = (%v,%v), want owned (data.data[] parse)", ok, err)
	}
}

func TestAPIVerifier_ForeignAgentRowRejected(t *testing.T) {
	// INV-4 defense-in-depth: a row whose agent_id is a DIFFERENT agent must not be
	// accepted even if the run_id matches (guards a mis-scoped server response).
	m := newMockBackend(t, 200, sessionsBodyForAgent("99999999-9999-9999-9999-999999999999", "sess-A"))
	v := m.verifier(t, 0)

	if ok, _ := v.OwnsSession(ctx, "sess-A"); ok {
		t.Fatal("a row owned by a different agent_id must be rejected (INV-4)")
	}
}

func TestAPIVerifier_RowMissingAgentIDNotOwned(t *testing.T) {
	// A row that omits agent_id is not proof of ownership — the INV-4 per-row check
	// is unconditional (a missing agent_id must not be treated as a match).
	m := newMockBackend(t, 200, `{"status":200,"data":{"data":[{"run_id":"sess-A"}]}}`)
	if ok, err := m.verifier(t, 0).OwnsSession(ctx, "sess-A"); ok || err != nil {
		t.Fatalf("a row without agent_id = (%v,%v), want (false,nil) not-owned (INV-4)", ok, err)
	}
}

func TestAPIVerifier_MatchingRowNotFirst(t *testing.T) {
	// The owned row is not the first in data[]; it must still be found.
	m := newMockBackend(t, 200, sessionsBody("other-run", "sess-A"))
	if ok, err := m.verifier(t, 0).OwnsSession(ctx, "sess-A"); !ok || err != nil {
		t.Fatalf("OwnsSession(sess-A) = (%v,%v), want owned even when not first", ok, err)
	}
}

func TestAPIVerifier_BroadenedSubstringNotOwned(t *testing.T) {
	// The ?search= ILIKE is a substring match, so the server may return a run_id
	// that merely CONTAINS the query. Exact equality must reject it.
	m := newMockBackend(t, 200, sessionsBody("sess-A-extra"))
	if ok, _ := m.verifier(t, 0).OwnsSession(ctx, "sess-A"); ok {
		t.Fatal("a superstring run_id must NOT match (exact equality only)")
	}
}

func TestAPIVerifier_DoesNotForwardKeyOnCrossHostRedirect(t *testing.T) {
	// G_SEC C1: a 302 to a foreign host must NOT forward the org key. With redirects
	// disabled the 302 surfaces as a non-2xx → fail-closed, and the foreign host is
	// never contacted (so the key never leaves the configured origin).
	var foreignHits int32
	foreign := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&foreignHits, 1)
		if r.Header.Get("X-API-Key") != "" {
			t.Errorf("org key leaked to foreign host via redirect: %q", r.Header.Get("X-API-Key"))
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(sessionsBody("sess-A"))) // would falsely "own" if followed
	}))
	defer foreign.Close()

	redirector := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+r.URL.Path, http.StatusFound)
	}))
	defer redirector.Close()

	v, err := NewAPIVerifier(APIVerifierConfig{
		BaseURL: redirector.URL, AgentID: testAgentID, PusherDID: testPusherDID(t), OrgAPIKey: "obx_key_secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := v.OwnsSession(ctx, "sess-A")
	if ok {
		t.Fatal("a redirect must not promote (must fail closed, not follow to the foreign body)")
	}
	if err == nil {
		t.Fatal("a 302 should surface as a fail-closed lookup error")
	}
	if n := atomic.LoadInt32(&foreignHits); n != 0 {
		t.Fatalf("foreign host was contacted %d times; the redirect must NOT be followed", n)
	}
}

func TestAPIVerifier_LookupErrorFailsClosed(t *testing.T) {
	m := newMockBackend(t, 500, `{"message":"boom"}`)
	v := m.verifier(t, 0)

	ok, err := v.OwnsSession(ctx, "sess-A")
	if ok {
		t.Fatal("a 5xx must NOT attribute (fail-closed)")
	}
	if err == nil {
		t.Fatal("a lookup fault should surface an error for logging")
	}
}

func TestAPIVerifier_AuthReject403FailsClosed(t *testing.T) {
	m := newMockBackend(t, 403, `{"message":"forbidden"}`)
	v := m.verifier(t, 0)

	if ok, _ := v.OwnsSession(ctx, "sess-A"); ok {
		t.Fatal("a 403 (bad/insufficient key) must NOT attribute")
	}
}

func TestAPIVerifier_MalformedBodyFailsClosed(t *testing.T) {
	m := newMockBackend(t, 200, `this is not json`)
	v := m.verifier(t, 0)

	ok, err := v.OwnsSession(ctx, "sess-A")
	if ok {
		t.Fatal("a malformed 200 body must NOT attribute")
	}
	if err == nil {
		t.Fatal("malformed body should surface an error")
	}
}

func TestAPIVerifier_WrongShapeBodyIsNotOwned(t *testing.T) {
	t.Run("missing data key", func(t *testing.T) {
		m := newMockBackend(t, 200, `{"sessions":[{"run_id":"sess-A"}]}`)
		if ok, err := m.verifier(t, 0).OwnsSession(ctx, "sess-A"); ok || err != nil {
			t.Fatalf("missing data key = (%v,%v), want (false,nil)", ok, err)
		}
	})
	t.Run("wrong-typed data", func(t *testing.T) {
		m := newMockBackend(t, 200, `{"data":"nope"}`)
		ok, err := m.verifier(t, 0).OwnsSession(ctx, "sess-A")
		if ok {
			t.Fatal("a wrong-typed data field must NOT attribute")
		}
		if err == nil {
			t.Fatal("wrong-typed data should surface a malformed-body error")
		}
	})
}

func TestAPIVerifier_TransportErrorFailsClosed(t *testing.T) {
	m := newMockBackend(t, 200, sessionsBody("sess-A"))
	v := m.verifier(t, 0)
	m.srv.Close() // kill the server before the read

	ok, err := v.OwnsSession(ctx, "sess-A")
	if ok {
		t.Fatal("a dead endpoint must NOT attribute")
	}
	if err == nil {
		t.Fatal("a transport fault should surface an error")
	}
}

func TestAPIVerifier_TimeoutFailsClosed(t *testing.T) {
	m := newMockBackend(t, 200, sessionsBody("sess-A"))
	m.delay = 200 * time.Millisecond
	v := m.verifier(t, 20*time.Millisecond)

	ok, err := v.OwnsSession(ctx, "sess-A")
	if ok {
		t.Fatal("a slow API must degrade to unverified, never over-attribute")
	}
	if err == nil {
		t.Fatal("a timeout should surface an error")
	}
}

// --- Request shape ----------------------------------------------------------

func TestAPIVerifier_SendsKeyedPathAndOrgKey(t *testing.T) {
	m := newMockBackend(t, 200, sessionsBody("sess-A"))
	v := m.verifier(t, 0)
	if _, err := v.OwnsSession(ctx, "sess-A"); err != nil {
		t.Fatal(err)
	}
	if got, want := m.lastReq.URL.Path, "/agent/"+testAgentID+"/sessions"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if got := m.lastReq.URL.Query().Get("search"); got != "sess-A" {
		t.Errorf("search = %q, want sess-A", got)
	}
	if got := m.lastReq.Header.Get("X-API-Key"); got != "obx_key_test" {
		t.Errorf("X-API-Key = %q, want obx_key_test", got)
	}
	// INV-1: the org key must never appear in the URL/path/query.
	if strings.Contains(m.lastReq.URL.String(), "obx_key_test") {
		t.Fatal("org key leaked into the URL")
	}
}

func TestAPIVerifier_EscapesUntrustedSessionID(t *testing.T) {
	// A trailer value with URL-significant chars must be query-escaped, never break
	// the URL or inject a second param.
	m := newMockBackend(t, 200, `{"status":200,"data":{"data":[]}}`)
	v := m.verifier(t, 0)
	nasty := "a&b=c d%25"
	if _, err := v.OwnsSession(ctx, nasty); err != nil {
		t.Fatal(err)
	}
	if got := m.lastReq.URL.Query().Get("search"); got != nasty {
		t.Errorf("search round-trip = %q, want %q (bad escaping)", got, nasty)
	}
	if _, dup := m.lastReq.URL.Query()["b"]; dup {
		t.Fatal("unescaped '&' injected a second query param")
	}
}

func TestAPIVerifier_CachesDefinitiveResultPerSession(t *testing.T) {
	m := newMockBackend(t, 200, sessionsBody("sess-A"))
	v := m.verifier(t, 0)

	for i := 0; i < 3; i++ {
		if _, err := v.OwnsSession(ctx, "sess-A"); err != nil {
			t.Fatal(err)
		}
	}
	if n := atomic.LoadInt32(&m.calls); n != 1 {
		t.Fatalf("queried the backend %d times for one id, want 1 (cached)", n)
	}
}

// --- Construction guards ----------------------------------------------------

func TestNewAPIVerifier_RejectsBadConfig(t *testing.T) {
	did := testPusherDID(t)
	cases := map[string]APIVerifierConfig{
		"no base url":        {AgentID: testAgentID, PusherDID: did, OrgAPIKey: "k"},
		"plaintext non-lb":   {BaseURL: "http://backend:3000", AgentID: testAgentID, PusherDID: did, OrgAPIKey: "k"},
		"no org key":         {BaseURL: "https://b", AgentID: testAgentID, PusherDID: did},
		"base url with path": {BaseURL: "https://b/api", AgentID: testAgentID, PusherDID: did, OrgAPIKey: "k"},
		"agent id not uuid":  {BaseURL: "https://b", AgentID: "not-a-uuid", PusherDID: did, OrgAPIKey: "k"},
		"agent id path-junk": {BaseURL: "https://b", AgentID: "1111111/1111/1111/1111/111111111111", PusherDID: did, OrgAPIKey: "k"},
		"did mismatch":       {BaseURL: "https://b", AgentID: testAgentID, PusherDID: "did:aip:00000000-0000-0000-0000-000000000000", OrgAPIKey: "k"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewAPIVerifier(cfg); err == nil {
				t.Fatal("expected a construction error for an unusable config")
			}
		})
	}
}

func TestNewAPIVerifier_BindsAgentIDToDID(t *testing.T) {
	// The happy pair (agentID derives to DID) constructs; swapping the DID for a
	// different agent's DID must be rejected — the verifier can only ever read the
	// deploy principal's own sessions (INV-4).
	if _, err := NewAPIVerifier(APIVerifierConfig{
		BaseURL: "https://b", AgentID: testAgentID, PusherDID: testPusherDID(t), OrgAPIKey: "k",
	}); err != nil {
		t.Fatalf("matching pair should construct: %v", err)
	}
	otherDID, _ := DIDForAgent("22222222-2222-2222-2222-222222222222")
	if _, err := NewAPIVerifier(APIVerifierConfig{
		BaseURL: "https://b", AgentID: testAgentID, PusherDID: otherDID, OrgAPIKey: "k",
	}); err == nil {
		t.Fatal("an agent id that names a different principal than the DID must be rejected")
	}
}

// --- Resolver integration (end-to-end via real git) -------------------------

func TestAPIVerifier_OwnedTrailerResolvesAttributed(t *testing.T) {
	m := newMockBackend(t, 200, sessionsBody("sess-A"))
	r := newTestRepo(t)
	sha := r.commit(trailerMsg("work", "sess-A"))

	res, err := r.resolver(m.verifier(t, 0)).Resolve(ctx, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusAttributed {
		t.Fatalf("status = %s, want attributed (owned session)", res.Status)
	}
	if !res.Sessions[0].Verified {
		t.Fatal("owned session not marked Verified")
	}
}

func TestAPIVerifier_ForgedTrailerStaysInferred(t *testing.T) {
	// SL5-SEC-1 for real: the pusher's agent owns sess-mine; the commit forges a
	// victim's id the agent does NOT own (backend returns no matching row).
	m := newMockBackend(t, 200, `{"data":[]}`) // search for sess-victim finds nothing
	r := newTestRepo(t)
	sha := r.commit(trailerMsg("work", "sess-victim"))

	res, err := r.resolver(m.verifier(t, 0)).Resolve(ctx, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusInferred {
		t.Fatalf("status = %s, want inferred (forged, unowned claim)", res.Status)
	}
	if res.Sessions[0].Verified {
		t.Fatal("forged sess-victim must NOT be Verified (SL5-SEC-1)")
	}
}

func TestAPIVerifier_LookupErrorResolvesInferred(t *testing.T) {
	m := newMockBackend(t, 503, `{}`)
	r := newTestRepo(t)
	sha := r.commit(trailerMsg("work", "sess-A"))

	res, err := r.resolver(m.verifier(t, 0)).Resolve(ctx, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusInferred {
		t.Fatalf("status = %s, want inferred (fail-closed on lookup error)", res.Status)
	}
	if res.Sessions[0].Verified {
		t.Fatal("claim marked Verified despite lookup error")
	}
}
