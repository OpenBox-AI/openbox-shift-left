package fakecore

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// v3AuthedRequest builds a POST to v3EvaluatePath the way the client does: a
// real bootstrap + RS256-signed exchange, then the obx_ API key and the
// exchanged workload bearer on the request itself -- so the fake's own auth
// check is exercised rather than bypassed.
func v3AuthedRequest(t *testing.T, f *Server, body []byte) *http.Request {
	t.Helper()
	ensureV3Identity()
	doc := fetchV3BootstrapDoc(t, f)
	clientID, _ := doc["client_id"].(string)
	tokenEndpoint, _ := doc["token_endpoint"].(string)
	assertion := signV3TestAssertion(t, v3PrivKey, doc, clientID, nil)
	status, resp := postV3Token(t, tokenEndpoint, clientID, assertion)
	if status != http.StatusOK {
		t.Fatalf("token exchange failed: %d %v", status, resp)
	}
	token, _ := resp["access_token"].(string)
	if token == "" {
		t.Fatal("token exchange returned no access_token")
	}

	req, err := http.NewRequest(http.MethodPost, f.URL()+v3EvaluatePath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+APIKey())
	req.Header.Set("X-OpenBox-Workload-Token", token)
	return req
}

// TestFakecoreNeverDedupes pins review rule 3, and it is load-bearing for more
// than tidiness: the gate/observe election test asserts that a call reaches the
// wire exactly once. A fake that collapsed two requests sharing an
// Idempotency-Key would answer "once" whichever way the election went, and
// that test would pass while proving nothing.
//
// The control plane does not dedupe developer events on their id either, which
// is why the election has to be right in the first place.
func TestFakecoreNeverDedupes(t *testing.T) {
	f := New(t, Script{})
	body := []byte(`{"source":"developer-runtime","event_type":"WorkflowStarted","workflow_id":"w","run_id":"r","timestamp":"2026-09-14T00:00:00Z"}`)

	for i := 0; i < 3; i++ {
		post(t, f, body, "the-same-key-every-time")
	}
	if n := len(f.Inbox()); n != 3 {
		t.Errorf("inbox holds %d of 3 identical requests; the fake is collapsing repeats and would hide a double delivery", n)
	}
}

// post authenticates and sends a request the way the client does, so the
// fake's own verification is exercised rather than bypassed.
func post(t *testing.T, f *Server, body []byte, idemKey string) {
	t.Helper()
	req := v3AuthedRequest(t, f, body)
	req.Header.Set("Idempotency-Key", idemKey)
	resp, err := (&http.Client{Transport: http.DefaultTransport}).Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; the fake refused a well-formed authenticated request: %v", resp.StatusCode, strings.Join(f.Rejections(), "; "))
	}
}

// postExpecting sends a correctly authenticated request and requires a given
// status, for the bodies the fake is meant to refuse on inspection.
func postExpecting(t *testing.T, f *Server, body []byte, want int) {
	t.Helper()
	resp, err := (&http.Client{Transport: http.DefaultTransport}).Do(v3AuthedRequest(t, f, body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d", resp.StatusCode, want)
	}
}

// TestFakecoreRefusesWhatCoreWouldRefuse proves the door is wired, not just
// that the predicates are correct.
//
// The predicates have their own unit test. What that cannot show is that the
// server consults them: disconnect the call and every scenario stays green,
// because a conformant binary never sends a body that would have been caught.
// The refusal path has to be exercised by something that deliberately sends a
// bad one.
func TestFakecoreRefusesWhatCoreWouldRefuse(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"a body that is not a JSON object", `"just a string"`, "not a JSON object"},
		{"a body carrying a spans key", `{"source":"developer-runtime","event_type":"WorkflowStarted","workflow_id":"w","run_id":"r","timestamp":"t","spans":[]}`, "wire shape"},
		{"a DevEvent type on the wire", `{"source":"developer-runtime","event_type":"tool_call","workflow_id":"w","run_id":"r","timestamp":"t"}`, "wire shape"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := New(t, Script{})
			postExpecting(t, f, []byte(tc.body), http.StatusBadRequest)
			if n := len(f.Inbox()); n != 0 {
				t.Errorf("the fake accepted %d malformed request(s)", n)
			}
			got := strings.Join(f.Rejections(), " | ")
			if !strings.Contains(got, tc.want) {
				t.Errorf("rejection reason %q does not mention %q", got, tc.want)
			}
			// INV-2 applies to test logs: a refusal never echoes the body.
			if strings.Contains(got, "developer-runtime") {
				t.Errorf("the rejection reason quoted the request body: %q", got)
			}
		})
	}
}

// TestFakecoreRefusesUnauthenticatedEvaluate covers each auth failure mode on
// its own, so a change that collapsed them into one check would be visible.
// There is no request signature to bend anymore -- a workload request
// carries no signed canonical string; the envelope is the obx_ API key on
// Authorization plus the exchanged workload bearer, and the fake must reject
// each independently of the other.
func TestFakecoreRefusesUnauthenticatedEvaluate(t *testing.T) {
	body := []byte(`{"source":"developer-runtime","event_type":"WorkflowStarted","workflow_id":"w","run_id":"r","timestamp":"t"}`)
	for _, tc := range []struct {
		name string
		bend func(h http.Header)
	}{
		{"wrong API key", func(h http.Header) { h.Set("Authorization", "Bearer obx_not_the_real_key") }},
		{"no workload token", func(h http.Header) { h.Del("X-OpenBox-Workload-Token") }},
		{"a token this server never issued", func(h http.Header) { h.Set("X-OpenBox-Workload-Token", "fake-workload-token-never-issued") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := New(t, Script{})
			req := v3AuthedRequest(t, f, body)
			tc.bend(req.Header)
			resp, err := (&http.Client{Transport: http.DefaultTransport}).Do(req)
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
			if n := len(f.Inbox()); n != 0 {
				t.Errorf("an unauthenticated request reached the inbox")
			}
		})
	}
}

// TestFakecoreRejectsV1Routes proves the deleted protocol is gone from the
// fake too: any /api/v1/* hit is answered (never left to hang) and recorded
// as a rejection named "v1 route", so a stale fixture or a client regression
// that still speaks it is loud rather than silently accepted.
func TestFakecoreRejectsV1Routes(t *testing.T) {
	for _, path := range []string{
		"/api/v1/governance/evaluate",
		"/api/v1/governance/approval",
		"/api/v1/auth/validate",
	} {
		t.Run(path, func(t *testing.T) {
			f := New(t, Script{})
			resp, err := http.Post(f.URL()+path, "application/json", bytes.NewReader([]byte(`{}`)))
			if err != nil {
				t.Fatalf("post %s: %v", path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404", resp.StatusCode)
			}
			got := strings.Join(f.Rejections(), " | ")
			if !strings.Contains(got, "v1 route") {
				t.Errorf("rejection reasons %q do not mention %q", got, "v1 route")
			}
			if n := len(f.Inbox()); n != 0 {
				t.Errorf("a v1 route hit reached the inbox")
			}
		})
	}
}
