package client

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

const testPrivateKeyB64 = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

const testDID = "did:aip:00000000-0000-0000-0000-000000000001"

func mustSigner(t *testing.T) *signer {
	t.Helper()
	s, err := newSigner(testDID, testPrivateKeyB64)
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	return s
}

func verifyLikeCore(pub ed25519.PublicKey, method, path string, body []byte, h http.Header) error {
	wantSHA := hex.EncodeToString(sha256Sum(body))
	if got := h.Get(headerBodySHA256); got != wantSHA {
		return fmt.Errorf("body sha mismatch: header %q vs computed %q", got, wantSHA)
	}
	canonical := strings.Join([]string{
		strings.ToUpper(method),
		path,
		h.Get(headerAgentTS),
		h.Get(headerAgentNonce),
		wantSHA,
	}, "\n")
	sig, err := base64.StdEncoding.DecodeString(h.Get(headerAgentSig))
	if err != nil {
		return fmt.Errorf("signature not std-base64: %v", err)
	}
	if !ed25519.Verify(pub, []byte(canonical), sig) {
		return fmt.Errorf("ed25519 verify failed")
	}
	if _, err := time.Parse(time.RFC3339Nano, h.Get(headerAgentTS)); err != nil {
		return fmt.Errorf("timestamp not RFC3339Nano-parseable: %v", err)
	}
	return nil
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

func TestSign_VerifiesAgainstKnownKeypair(t *testing.T) {
	s := mustSigner(t)
	pub := s.priv.Public().(ed25519.PublicKey)
	body := []byte(`{"event_type":"ToolCall","run_id":"s1"}`)
	now := time.Date(2026, 7, 8, 12, 0, 0, 500000000, time.UTC)

	sig, err := s.sign(http.MethodPost, evaluatePath, body, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if want := hex.EncodeToString(sha256Sum(body)); sig.bodySHA != want {
		t.Errorf("bodySHA = %q, want %q", sig.bodySHA, want)
	}
	if !strings.HasSuffix(sig.timestamp, "+00:00") {
		t.Errorf("timestamp %q lacks +00:00 offset", sig.timestamp)
	}
	if _, err := time.Parse(time.RFC3339Nano, sig.timestamp); err != nil {
		t.Errorf("timestamp not RFC3339Nano: %v", err)
	}
	if len(sig.nonce) != 32 {
		t.Errorf("nonce len = %d, want 32", len(sig.nonce))
	}
	if _, err := base64.RawURLEncoding.DecodeString(sig.nonce); err != nil {
		t.Errorf("nonce not base64url-nopad: %v", err)
	}

	h := http.Header{}
	h.Set(headerAgentTS, sig.timestamp)
	h.Set(headerAgentNonce, sig.nonce)
	h.Set(headerAgentSig, sig.sig)
	h.Set(headerBodySHA256, sig.bodySHA)
	if err := verifyLikeCore(pub, http.MethodPost, evaluatePath, body, h); err != nil {
		t.Errorf("core-mirror verification failed: %v", err)
	}
}

func TestSign_TamperedBodyFailsVerification(t *testing.T) {
	s := mustSigner(t)
	pub := s.priv.Public().(ed25519.PublicKey)
	body := []byte(`{"a":1}`)
	sig, err := s.sign(http.MethodPost, evaluatePath, body, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	h := http.Header{}
	h.Set(headerAgentTS, sig.timestamp)
	h.Set(headerAgentNonce, sig.nonce)
	h.Set(headerAgentSig, sig.sig)
	h.Set(headerBodySHA256, sig.bodySHA)

	if err := verifyLikeCore(pub, http.MethodPost, evaluatePath, []byte(`{"a":2}`), h); err == nil {
		t.Fatal("expected verification failure for tampered body, got nil")
	}
}

func TestNewSigner_RejectsBadSeed(t *testing.T) {
	if _, err := newSigner(testDID, "not-base64!!"); err == nil {
		t.Error("expected error for non-base64 seed")
	}
	short := base64.StdEncoding.EncodeToString(make([]byte, 31))
	if _, err := newSigner(testDID, short); err == nil {
		t.Error("expected error for 31-byte seed")
	}
}

func TestCanonicalString_Shape(t *testing.T) {
	got := canonicalString("post", "/p", "2026-01-01T00:00:00Z", "nnn", "deadbeef")
	want := "POST\n/p\n2026-01-01T00:00:00Z\nnnn\ndeadbeef"
	if got != want {
		t.Errorf("canonical = %q, want %q", got, want)
	}
	if strings.HasSuffix(got, "\n") {
		t.Error("canonical must not have a trailing newline")
	}
}
