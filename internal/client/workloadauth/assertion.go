package workloadauth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// assertionLifetime is the client assertion's fixed lifetime: SDK behaviour 2
// pins this at exactly 60 seconds, which is also what the exchange
// rejection's clock-skew hint (errors.go) assumes.
const assertionLifetime = 60 * time.Second

// jtiRandomBytes is the number of random bytes base64url-encoded into jti.
const jtiRandomBytes = 24

// BuildAssertion builds the RS256 client-assertion JWT the token exchange
// signs: header {alg:RS256, kid, typ:JWT}, claims
// {aud:token_endpoint, exp:iat+60, iat, iss:client_id, jti, sub:client_id}.
//
// jwt.MapClaims is used rather than jwt.RegisteredClaims deliberately: v5's
// RegisteredClaims marshals a single-string "aud" as a one-element JSON
// array by default (MarshalSingleStringAsArray), and the SDK this mirrors
// emits "aud" as a plain string. Go's json.Marshal of a map sorts keys and
// uses compact separators, which is the same canonical form the SDK
// produces.
func BuildAssertion(key *rsa.PrivateKey, doc *BootstrapDocument) (string, error) {
	jti, err := randomJTI()
	if err != nil {
		return "", fmt.Errorf("workloadauth: building assertion jti: %w", err)
	}

	now := time.Now().UTC()
	claims := jwt.MapClaims{
		"aud": doc.TokenEndpoint,
		"exp": now.Add(assertionLifetime).Unix(),
		"iat": now.Unix(),
		"iss": doc.ClientID,
		"jti": jti,
		"sub": doc.ClientID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = doc.Kid

	signed, err := token.SignedString(key)
	if err != nil {
		return "", fmt.Errorf("workloadauth: signing assertion: %w", err)
	}
	return signed, nil
}

// randomJTI is base64url(no padding) over jtiRandomBytes of crypto/rand
// output, mirroring the SDK's secrets.token_urlsafe(24).
func randomJTI() (string, error) {
	buf := make([]byte, jtiRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
