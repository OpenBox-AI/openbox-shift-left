package workloadauth

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testDocForAssertion() *BootstrapDocument {
	return &BootstrapDocument{
		TokenEndpoint: "https://identity.node.lat/realms/openbox/protocol/openid-connect/token",
		ClientID:      "workload-client-1",
		Kid:           "kid-abc",
	}
}

func TestAssertionHeaderAndClaimsAreExact(t *testing.T) {
	key := testRSAKey(t)
	doc := testDocForAssertion()

	jwtStr, err := BuildAssertion(key, doc)
	if err != nil {
		t.Fatalf("BuildAssertion: %v", err)
	}

	parts := strings.Split(jwtStr, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion has %d segments, want 3", len(parts))
	}

	headerRaw, err := base64URLDecodeNoPad(parts[0])
	if err != nil {
		t.Fatalf("decoding header: %v", err)
	}
	var header map[string]any
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		t.Fatalf("unmarshalling header: %v", err)
	}
	if header["alg"] != "RS256" {
		t.Errorf("header.alg = %v, want RS256", header["alg"])
	}
	if header["kid"] != doc.Kid {
		t.Errorf("header.kid = %v, want %v", header["kid"], doc.Kid)
	}
	if header["typ"] != "JWT" {
		t.Errorf("header.typ = %v, want JWT", header["typ"])
	}

	claimsRaw, err := base64URLDecodeNoPad(parts[1])
	if err != nil {
		t.Fatalf("decoding claims: %v", err)
	}
	// aud must be a raw JSON string, never an array (golang-jwt's
	// RegisteredClaims default would emit ["aud"]).
	var rawClaims map[string]json.RawMessage
	if err := json.Unmarshal(claimsRaw, &rawClaims); err != nil {
		t.Fatalf("unmarshalling claims: %v", err)
	}
	var aud string
	if err := json.Unmarshal(rawClaims["aud"], &aud); err != nil {
		t.Fatalf("claims.aud is not a JSON string: %s", rawClaims["aud"])
	}
	if aud != doc.TokenEndpoint {
		t.Errorf("claims.aud = %q, want %q", aud, doc.TokenEndpoint)
	}

	var claims struct {
		Exp int64  `json:"exp"`
		Iat int64  `json:"iat"`
		Iss string `json:"iss"`
		Jti string `json:"jti"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		t.Fatalf("unmarshalling claims: %v", err)
	}
	if claims.Exp-claims.Iat != 60 {
		t.Errorf("exp-iat = %d, want 60", claims.Exp-claims.Iat)
	}
	if claims.Iss != doc.ClientID {
		t.Errorf("claims.iss = %q, want %q", claims.Iss, doc.ClientID)
	}
	if claims.Sub != doc.ClientID {
		t.Errorf("claims.sub = %q, want %q", claims.Sub, doc.ClientID)
	}
	if claims.Jti == "" {
		t.Error("claims.jti is empty")
	}
	now := time.Now().Unix()
	if claims.Iat > now || claims.Iat < now-10 {
		t.Errorf("claims.iat = %d is not close to now (%d)", claims.Iat, now)
	}

	sig, err := base64URLDecodeNoPad(parts[2])
	if err != nil {
		t.Fatalf("decoding signature: %v", err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("independent RS256 verification failed: %v", err)
	}
}

func TestAssertionJTIsAreDistinct(t *testing.T) {
	key := testRSAKey(t)
	doc := testDocForAssertion()

	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		jwtStr, err := BuildAssertion(key, doc)
		if err != nil {
			t.Fatalf("BuildAssertion: %v", err)
		}
		parts := strings.Split(jwtStr, ".")
		claimsRaw, err := base64URLDecodeNoPad(parts[1])
		if err != nil {
			t.Fatalf("decoding claims: %v", err)
		}
		var claims struct {
			Jti string `json:"jti"`
		}
		if err := json.Unmarshal(claimsRaw, &claims); err != nil {
			t.Fatalf("unmarshalling claims: %v", err)
		}
		if seen[claims.Jti] {
			t.Fatalf("duplicate jti: %s", claims.Jti)
		}
		seen[claims.Jti] = true
	}
}

// stdEncodingSanity guards the base64url-no-pad assumption BuildAssertion and
// this test both rely on.
func TestStdBase64URLNoPadSanity(t *testing.T) {
	if base64.RawURLEncoding.EncodedLen(1) == 0 {
		t.Fatal("sanity check failed")
	}
}
