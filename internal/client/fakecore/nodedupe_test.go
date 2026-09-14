package fakecore

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

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

// post signs and sends a request the way the client does, so the fake's own
// verification is exercised rather than bypassed.
func post(t *testing.T, f *Server, body []byte, idemKey string) {
	t.Helper()
	req := signedRequest(t, f, body)
	req.Header.Set("Idempotency-Key", idemKey)
	resp, err := (&http.Client{Transport: http.DefaultTransport}).Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; the fake refused a well-formed signed request: %v", resp.StatusCode, strings.Join(f.Rejections(), "; "))
	}
}

// postExpecting sends a correctly signed request and requires a given status,
// for the bodies the fake is meant to refuse on inspection.
func postExpecting(t *testing.T, f *Server, body []byte, want int) {
	t.Helper()
	resp, err := (&http.Client{Transport: http.DefaultTransport}).Do(signedRequest(t, f, body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d", resp.StatusCode, want)
	}
}

// signedRequest builds a request signed the way the client signs, so a test
// bending one header changes exactly one thing.
func signedRequest(t *testing.T, f *Server, body []byte) *http.Request {
	t.Helper()
	seed, err := base64.StdEncoding.DecodeString(f.SeedB64())
	if err != nil {
		t.Fatalf("decode seed: %v", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	sum := sha256.Sum256(body)
	bodySHA := hex.EncodeToString(sum[:])
	const ts, nonce = "2026-09-14T00:00:00Z", "n"
	canonical := "POST\n" + evaluatePath + "\n" + ts + "\n" + nonce + "\n" + bodySHA

	req, err := http.NewRequest(http.MethodPost, f.URL()+evaluatePath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set(hdrAgentDID, f.DID())
	req.Header.Set(hdrAgentTS, ts)
	req.Header.Set(hdrAgentNonce, nonce)
	req.Header.Set(hdrBodySHA, bodySHA)
	req.Header.Set(hdrAgentSig, base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(canonical))))

	return req
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

// TestFakecoreRefusesABadSignature covers each verification failure on its own,
// so a change that collapsed them into one check would be visible.
func TestFakecoreRefusesABadSignature(t *testing.T) {
	body := []byte(`{"source":"developer-runtime","event_type":"WorkflowStarted","workflow_id":"w","run_id":"r","timestamp":"t"}`)
	for _, tc := range []struct {
		name string
		bend func(http.Header)
		want string
	}{
		{"the digest header does not match the body", func(h http.Header) { h.Set(hdrBodySHA, strings.Repeat("0", 64)) }, "body digest"},
		{"no agent DID", func(h http.Header) { h.Set(hdrAgentDID, "") }, "agent DID"},
		{"a signature that is not base64", func(h http.Header) { h.Set(hdrAgentSig, "!!!not base64!!!") }, "not base64"},
		{"a signature from a key the fake does not know", func(h http.Header) { h.Set(hdrAgentSig, base64.StdEncoding.EncodeToString(make([]byte, 64))) }, "does not verify"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := New(t, Script{})
			req := signedRequest(t, f, body)
			tc.bend(req.Header)
			resp, err := (&http.Client{Transport: http.DefaultTransport}).Do(req)
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
			if got := strings.Join(f.Rejections(), " | "); !strings.Contains(got, tc.want) {
				t.Errorf("rejection reason %q does not mention %q", got, tc.want)
			}
		})
	}
}
