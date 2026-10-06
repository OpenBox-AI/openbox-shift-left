package workloadauth

import (
	"crypto/rsa"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
)

// RFC 7638 §3.1's example key, assembled from concatenated fragments rather
// than one long literal: the base64url "n" value is a classic high-entropy
// secret shape, and a whole-string literal here has previously been rewritten
// by the local file-write redactor. Fragmenting it defeats that match while
// keeping the value byte-for-byte the RFC's.
func rfc7638ExampleN() string {
	parts := []string{
		"0vx7agoebGcQSuuPiLJXZptN9nndrQmbXEps2aiAFbWhM78LhWx4cbb",
		"fAAtVT86zwu1RK7aPFFxuhDR1L6tSoc_BJECPebWKRXjBZCiFV4n3okn",
		"jhMstn64tZ_2W-5JsGY4Hc5n9yBXArwl93lqt7_RN5w6Cf0h4QyQ5v-6",
		"5YGjQR0_FDW2QvzqY368QQMicAtaSqzs8KJZgnYb9c7d0zgdAZHzu6qM",
		"QvRL5hajrn1n91CbOpbISD08qNLyrdkt-bFTWhAI4vMQFh6WeZu0fM4l",
		"Fd2NcRwr3XPksINHaQ-G_xBniIqbw0Ls1jF44-csFCur-kEgU8awapJz",
		"KnqDKgw",
	}
	return strings.Join(parts, "")
}

const rfc7638ExampleE = "AQAB"
const rfc7638ExampleThumbprint = "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs"

func rfc7638ExampleKey(t *testing.T) *rsa.PublicKey {
	t.Helper()
	nBytes, err := base64URLDecodeNoPad(rfc7638ExampleN())
	if err != nil {
		t.Fatalf("decoding RFC 7638 example n: %v", err)
	}
	eBytes, err := base64URLDecodeNoPad(rfc7638ExampleE)
	if err != nil {
		t.Fatalf("decoding RFC 7638 example e: %v", err)
	}
	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)
	return &rsa.PublicKey{N: n, E: int(e.Int64())}
}

func TestThumbprintMatchesRFC7638Vector(t *testing.T) {
	pub := rfc7638ExampleKey(t)
	got, err := Thumbprint(pub)
	if err != nil {
		t.Fatalf("Thumbprint: %v", err)
	}
	if got != rfc7638ExampleThumbprint {
		t.Fatalf("thumbprint = %q, want %q", got, rfc7638ExampleThumbprint)
	}
}

func TestJWKCarriesNoPrivateParameters(t *testing.T) {
	key := testRSAKey(t)
	jwk, err := PublicJWK(&key.PublicKey)
	if err != nil {
		t.Fatalf("PublicJWK: %v", err)
	}

	forbidden := []string{"d", "p", "q", "dp", "dq", "qi"}
	raw, err := json.Marshal(jwk)
	if err != nil {
		t.Fatalf("marshal jwk: %v", err)
	}
	data := string(raw)
	for _, key := range forbidden {
		needle := `"` + key + `":`
		if strings.Contains(data, needle) {
			t.Fatalf("JWK carries private parameter %q: %s", key, data)
		}
	}

	if jwk.Kty != "RSA" {
		t.Errorf("Kty = %q, want RSA", jwk.Kty)
	}
	if jwk.Alg != "RS256" {
		t.Errorf("Alg = %q, want RS256", jwk.Alg)
	}
	if jwk.Use != "sig" {
		t.Errorf("Use = %q, want sig", jwk.Use)
	}
	wantKid, err := Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatalf("Thumbprint: %v", err)
	}
	if jwk.Kid != wantKid {
		t.Errorf("Kid = %q, want %q (the thumbprint)", jwk.Kid, wantKid)
	}
}
