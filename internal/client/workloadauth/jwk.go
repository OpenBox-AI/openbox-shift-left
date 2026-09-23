package workloadauth

import (
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/big"
)

// JWK is the public half of the workload key, in the shape the SDK's
// bootstrap/registration flow expects: RSA, sig use, RS256, with kid set to
// the RFC 7638 thumbprint. It carries no private parameter.
type JWK struct {
	Kty string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

// PublicJWK returns pub's public JWK, keyed by its own RFC 7638 thumbprint.
func PublicJWK(pub *rsa.PublicKey) (JWK, error) {
	kid, err := Thumbprint(pub)
	if err != nil {
		return JWK{}, err
	}
	return JWK{
		Kty: "RSA",
		N:   rsaJWKModulus(pub),
		E:   rsaJWKExponent(pub),
		Kid: kid,
		Alg: "RS256",
		Use: "sig",
	}, nil
}

// Thumbprint computes pub's RFC 7638 §3.1 JWK thumbprint: SHA-256 over the
// canonical JSON object of pub's REQUIRED members only, in lexicographic
// member-name order ("e", "kty", "n"), no insignificant whitespace, encoded
// base64url without padding.
func Thumbprint(pub *rsa.PublicKey) (string, error) {
	if pub == nil {
		return "", fmt.Errorf("workloadauth: nil public key")
	}
	canonical := fmt.Sprintf(`{"e":%q,"kty":"RSA","n":%q}`, rsaJWKExponent(pub), rsaJWKModulus(pub))
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// rsaJWKModulus is pub.N as an unsigned big-endian base64url integer, per
// RFC 7518 §6.3.1.
func rsaJWKModulus(pub *rsa.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
}

// rsaJWKExponent is pub.E as an unsigned big-endian base64url integer, per
// RFC 7518 §6.3.1.
func rsaJWKExponent(pub *rsa.PublicKey) string {
	e := big.NewInt(int64(pub.E))
	return base64.RawURLEncoding.EncodeToString(e.Bytes())
}

// base64URLDecodeNoPad decodes s as unpadded base64url, the encoding every
// JWK member and JWT segment in this package uses.
func base64URLDecodeNoPad(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}
