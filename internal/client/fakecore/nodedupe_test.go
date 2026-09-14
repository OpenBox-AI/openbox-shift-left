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
