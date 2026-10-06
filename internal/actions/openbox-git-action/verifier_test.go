package gitaction

import (
	"context"
	"errors"
	"net/http"

	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
)

const testAgentID = "11111111-1111-1111-1111-111111111111"

func testPusherDID(t *testing.T) string {
	t.Helper()
	did, err := devconfig.AttributionDIDFor(testAgentID)
	if err != nil {
		t.Fatalf("AttributionDIDFor: %v", err)
	}
	return did
}

// witnessOK returns a Witness that reports agentID, unconditionally, as
// though core's own /api/v3/auth/validate had just confirmed it.
func witnessOK(agentID string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return agentID, nil }
}

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

func (m *mockBackend) verifier(t *testing.T, timeout time.Duration) OwnershipVerifier {
	t.Helper()
	v, err := NewAPIVerifier(APIVerifierConfig{
		BaseURL:   m.srv.URL,
		AgentID:   testAgentID,
		Witness:   witnessOK(testAgentID),
		OrgAPIKey: "obx_key_test",
		Timeout:   timeout,
	})
	if err != nil {
		t.Fatalf("NewAPIVerifier: %v", err)
	}
	return v
}

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
	m := newMockBackend(t, 200, `{"status":200,"data":{"data":[{"id":"sess-A","run_id":"other-run","agent_id":"`+testAgentID+`"}]}}`)
	v := m.verifier(t, 0)

	if ok, _ := v.OwnsSession(ctx, "sess-A"); ok {
		t.Fatal("matched on id PK; must match run_id only")
	}
}

func TestAPIVerifier_ParsesRealBackendEnvelope(t *testing.T) {
	// A body in the running backend's exact envelope shape must resolve as owned.
	body := `{"status":200,"data":{"data":[{"id":"2c4e6a80-1b3d-4f5a-9c7e-0d2f4a6b8c01",` +
		`"agent_id":"` + testAgentID + `","workflow_id":"` + testPusherDID(t) + `",` +
		`"run_id":"3d5f7b91-2c4e-4a6b-8d0f-1e3a5b7c9d02","status":"completed",` +
		`"metadata":null,"current_step":{"event_type":"SessionStarted"}}],"total":1,"page":1}}`
	m := newMockBackend(t, 200, body)
	if ok, err := m.verifier(t, 0).OwnsSession(ctx, "3d5f7b91-2c4e-4a6b-8d0f-1e3a5b7c9d02"); !ok || err != nil {
		t.Fatalf("real backend envelope = (%v,%v), want owned (data.data[] parse)", ok, err)
	}
}

func TestAPIVerifier_ForeignAgentRowRejected(t *testing.T) {
	m := newMockBackend(t, 200, sessionsBodyForAgent("99999999-9999-9999-9999-999999999999", "sess-A"))
	v := m.verifier(t, 0)

	if ok, _ := v.OwnsSession(ctx, "sess-A"); ok {
		t.Fatal("a row owned by a different agent_id must be rejected: identity comes from the caller, never the body")
	}
}

func TestAPIVerifier_RowMissingAgentIDNotOwned(t *testing.T) {
	m := newMockBackend(t, 200, `{"status":200,"data":{"data":[{"run_id":"sess-A"}]}}`)
	if ok, err := m.verifier(t, 0).OwnsSession(ctx, "sess-A"); ok || err != nil {
		t.Fatalf("a row without agent_id = (%v,%v), want (false,nil) not-owned", ok, err)
	}
}

func TestAPIVerifier_MatchingRowNotFirst(t *testing.T) {
	m := newMockBackend(t, 200, sessionsBody("other-run", "sess-A"))
	if ok, err := m.verifier(t, 0).OwnsSession(ctx, "sess-A"); !ok || err != nil {
		t.Fatalf("OwnsSession(sess-A) = (%v,%v), want owned even when not first", ok, err)
	}
}

func TestAPIVerifier_BroadenedSubstringNotOwned(t *testing.T) {
	// Exact equality must reject it.
	m := newMockBackend(t, 200, sessionsBody("sess-A-extra"))
	if ok, _ := m.verifier(t, 0).OwnsSession(ctx, "sess-A"); ok {
		t.Fatal("a superstring run_id must NOT match (exact equality only)")
	}
}

func TestAPIVerifier_DoesNotForwardKeyOnCrossHostRedirect(t *testing.T) {
	// With redirects disabled the 302 surfaces as a non-2xx → fail-closed, and
	// the foreign host is never contacted (so the key never leaves the configured
	// origin).
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
		BaseURL: redirector.URL, AgentID: testAgentID, Witness: witnessOK(testAgentID), OrgAPIKey: "obx_key_secret",
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
	if strings.Contains(m.lastReq.URL.String(), "obx_key_test") {
		t.Fatal("org key leaked into the URL")
	}
}

func TestAPIVerifier_EscapesUntrustedSessionID(t *testing.T) {
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

func TestNewAPIVerifier_RejectsBadConfig(t *testing.T) {
	witness := witnessOK(testAgentID)
	cases := map[string]APIVerifierConfig{
		"no base url":        {AgentID: testAgentID, Witness: witness, OrgAPIKey: "k"},
		"plaintext non-lb":   {BaseURL: "http://backend:3000", AgentID: testAgentID, Witness: witness, OrgAPIKey: "k"},
		"no org key":         {BaseURL: "https://b", AgentID: testAgentID, Witness: witness},
		"base url with path": {BaseURL: "https://b/api", AgentID: testAgentID, Witness: witness, OrgAPIKey: "k"},
		"agent id not uuid":  {BaseURL: "https://b", AgentID: "not-a-uuid", Witness: witness, OrgAPIKey: "k"},
		"agent id path-junk": {BaseURL: "https://b", AgentID: "1111111/1111/1111/1111/111111111111", Witness: witness, OrgAPIKey: "k"},
		"no witness":         {BaseURL: "https://b", AgentID: testAgentID, OrgAPIKey: "k"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewAPIVerifier(cfg); err == nil {
				t.Fatal("expected a construction error for an unusable config")
			}
		})
	}
}

// TestOwnershipWitnessMatchPasses an uppercase-vs-lowercase UUID rendering of
// the same agent must still match: the compare is on parsed UUIDs, not raw
// strings.
func TestOwnershipWitnessMatchPasses(t *testing.T) {
	if _, err := NewAPIVerifier(APIVerifierConfig{
		BaseURL: "https://b", AgentID: strings.ToUpper(testAgentID), Witness: witnessOK(testAgentID), OrgAPIKey: "k",
	}); err != nil {
		t.Fatalf("a witness naming the same agent (different case) should construct: %v", err)
	}
}

// TestOwnershipWitnessMismatchRefusesAnotherPrincipal core's witness naming a
// different agent than the one configured must refuse construction with the
// existing "another principal's sessions" wording -- this is what replaced
// the tautological DID bind.
func TestOwnershipWitnessMismatchRefusesAnotherPrincipal(t *testing.T) {
	other := "22222222-2222-2222-2222-222222222222"
	_, err := NewAPIVerifier(APIVerifierConfig{
		BaseURL: "https://b", AgentID: testAgentID, Witness: witnessOK(other), OrgAPIKey: "k",
	})
	if err == nil {
		t.Fatal("a witness naming a different agent must be refused")
	}
	if !strings.Contains(err.Error(), "refusing to read another principal's sessions") {
		t.Fatalf("error = %q, want the existing refusal wording", err)
	}
}

// TestOwnershipWitness401Refuses any witness error, a 401 included, refuses
// construction; the caller (selectVerifier) degrades to NoopVerifier.
func TestOwnershipWitness401Refuses(t *testing.T) {
	_, err := NewAPIVerifier(APIVerifierConfig{
		BaseURL: "https://b", AgentID: testAgentID, OrgAPIKey: "k",
		Witness: func(context.Context) (string, error) {
			return "", errors.New("auth/validate returned HTTP 401: identity rejected")
		},
	})
	if err == nil {
		t.Fatal("a 401 from the witness must refuse construction")
	}
}

// TestNoSessionReadBeforeWitness a witness failure must never let a session
// read happen: construction fails before the returned verifier (there is
// none) could ever be asked OwnsSession, so the session backend must see zero
// requests.
func TestNoSessionReadBeforeWitness(t *testing.T) {
	m := newMockBackend(t, 200, sessionsBody("sess-A"))
	_, err := NewAPIVerifier(APIVerifierConfig{
		BaseURL: m.srv.URL, AgentID: testAgentID, OrgAPIKey: "k",
		Witness: func(context.Context) (string, error) { return "", errors.New("witness unreachable") },
	})
	if err == nil {
		t.Fatal("expected construction to fail when the witness errors")
	}
	if n := atomic.LoadInt32(&m.calls); n != 0 {
		t.Fatalf("the session backend was hit %d times despite the witness failing; ownership must never be "+
			"read before the witness succeeds", n)
	}
}

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
		t.Fatal("forged sess-victim must NOT be Verified")
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
