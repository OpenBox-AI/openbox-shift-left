package fakecore

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// fetchV3BootstrapDoc drives the real GET, using the process-wide v3 API key,
// and decodes the document with stdlib JSON only -- never through
// workloadauth, which the import wall (guard_test.go) forbids from this
// package.
func fetchV3BootstrapDoc(t *testing.T, fc *Server) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fc.URL()+v3BootstrapPath, nil)
	if err != nil {
		t.Fatalf("build bootstrap request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+APIKey())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET bootstrap: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("bootstrap status = %d, body = %s", resp.StatusCode, body)
	}
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode bootstrap doc: %v", err)
	}
	return doc
}

// signV3TestAssertion builds and RS256-signs a client assertion by hand
// (stdlib crypto/rsa, not golang-jwt), matching workloadauth.BuildAssertion's
// claim set. mutate, if non-nil, tampers with the claims after they are built
// but before signing.
func signV3TestAssertion(t *testing.T, key *rsa.PrivateKey, doc map[string]any, clientID string, mutate func(claims map[string]any)) string {
	t.Helper()
	header := map[string]any{"alg": "RS256", "kid": doc["kid"], "typ": "JWT"}
	now := time.Now().UTC()
	claims := map[string]any{
		"aud": doc["token_endpoint"],
		"iss": clientID,
		"sub": clientID,
		"iat": now.Unix(),
		"exp": now.Add(60 * time.Second).Unix(),
		"jti": fmt.Sprintf("test-jti-%d", now.UnixNano()),
	}
	if mutate != nil {
		mutate(claims)
	}
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal assertion header: %v", err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal assertion claims: %v", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatalf("sign test assertion: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func postV3Token(t *testing.T, tokenEndpoint, clientID, assertion string) (int, map[string]any) {
	t.Helper()
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {clientID},
		"client_assertion_type": {v3ClientAssertionType},
		"client_assertion":      {assertion},
	}
	resp, err := http.PostForm(tokenEndpoint, form)
	if err != nil {
		t.Fatalf("POST token: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	return resp.StatusCode, m
}

// TestFakecoreVerifiesAssertionIndependently proves the fake's token endpoint
// actually checks the RS256 signature and the claims, rather than trusting
// anything the caller asserts about itself: a valid assertion is accepted, one
// signed by an unrelated key is not, and one whose audience does not name this
// server's own token endpoint is not either.
func TestFakecoreVerifiesAssertionIndependently(t *testing.T) {
	ensureV3Identity()
	fc := New(t, Script{})
	doc := fetchV3BootstrapDoc(t, fc)
	tokenEndpoint, _ := doc["token_endpoint"].(string)
	clientID, _ := doc["client_id"].(string)

	valid := signV3TestAssertion(t, v3PrivKey, doc, clientID, nil)
	if status, body := postV3Token(t, tokenEndpoint, clientID, valid); status != http.StatusOK {
		t.Fatalf("a validly-signed, correctly-audienced assertion was rejected: %d %v", status, body)
	} else if body["access_token"] == "" {
		t.Fatal("a successful exchange carried no access_token")
	}

	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate an unrelated key: %v", err)
	}
	wrongSig := signV3TestAssertion(t, otherKey, doc, clientID, nil)
	if status, _ := postV3Token(t, tokenEndpoint, clientID, wrongSig); status == http.StatusOK {
		t.Error("an assertion signed by a key other than the one bootstrap named was accepted")
	}

	wrongAud := signV3TestAssertion(t, v3PrivKey, doc, clientID, func(c map[string]any) {
		c["aud"] = "https://not-this-servers-token-endpoint.example/protocol/openid-connect/token"
	})
	if status, _ := postV3Token(t, tokenEndpoint, clientID, wrongAud); status == http.StatusOK {
		t.Error("an assertion with the wrong audience claim was accepted")
	}
}

// TestFakecoreCountsBootstrapAndExchange pins the two counters a warm/cold
// client-side test relies on to prove it did or did not re-authenticate.
func TestFakecoreCountsBootstrapAndExchange(t *testing.T) {
	ensureV3Identity()
	fc := New(t, Script{})

	if got := fc.BootstrapHits(); got != 0 {
		t.Fatalf("BootstrapHits = %d before any call, want 0", got)
	}
	if got := fc.ExchangeHits(); got != 0 {
		t.Fatalf("ExchangeHits = %d before any call, want 0", got)
	}

	doc := fetchV3BootstrapDoc(t, fc)
	if got := fc.BootstrapHits(); got != 1 {
		t.Errorf("BootstrapHits = %d after one bootstrap, want 1", got)
	}
	if got := fc.ExchangeHits(); got != 0 {
		t.Errorf("ExchangeHits = %d after a bootstrap-only call, want 0", got)
	}

	clientID, _ := doc["client_id"].(string)
	tokenEndpoint, _ := doc["token_endpoint"].(string)
	assertion := signV3TestAssertion(t, v3PrivKey, doc, clientID, nil)
	if status, body := postV3Token(t, tokenEndpoint, clientID, assertion); status != http.StatusOK {
		t.Fatalf("token exchange failed: %d %v", status, body)
	}
	if got := fc.ExchangeHits(); got != 1 {
		t.Errorf("ExchangeHits = %d after one exchange, want 1", got)
	}
	if got := fc.BootstrapHits(); got != 1 {
		t.Errorf("BootstrapHits = %d after an exchange, want it unmoved at 1", got)
	}

	fetchV3BootstrapDoc(t, fc)
	if got := fc.BootstrapHits(); got != 2 {
		t.Errorf("BootstrapHits = %d after a second bootstrap, want 2", got)
	}
	if got := fc.ExchangeHits(); got != 1 {
		t.Errorf("ExchangeHits = %d after a bootstrap-only call, want it unmoved at 1", got)
	}
}
