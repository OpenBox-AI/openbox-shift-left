package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// AIP signing header names (verified byte-for-byte against the reference
// implementation openbox-temporal-sdk-python/openbox/request_signing.py, which
// the docstring pins to openbox-core agent.go's verifier).
const (
	headerAuthorization = "Authorization"
	headerSDKVersion    = "X-OpenBox-SDK-Version"
	headerUserAgent     = "User-Agent"
	headerAgentDID      = "X-OpenBox-Agent-DID"
	headerAgentTS       = "X-OpenBox-Agent-Timestamp"
	headerAgentNonce    = "X-OpenBox-Agent-Nonce"
	headerAgentSig      = "X-OpenBox-Agent-Signature"
	headerBodySHA256    = "X-OpenBox-Body-SHA256"

	// headerIdempotencyKey carries the event's idempotency key (==
	// DevEvent.EventID == metadata.event_id) as a standard request header
	// (INV-5).
	headerIdempotencyKey = "Idempotency-Key"
)

const sdkVersion = "openbox-shift-left/0.1.0"

// signer holds an agent's AIP Ed25519 identity. The seed is never logged or
// exposed (INV-1); only the derived signatures leave this type.
type signer struct {
	did  string
	priv ed25519.PrivateKey
}

func newSigner(did, seedB64 string) (*signer, error) {
	if !strings.HasPrefix(did, "did:aip:") {
		return nil, fmt.Errorf("agent DID must start with did:aip:, got %q", did)
	}
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil {
		return nil, fmt.Errorf("decode Ed25519 seed: %w", err)
	}
	if len(seed) != ed25519.SeedSize { // 32
		return nil, fmt.Errorf("Ed25519 seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	return &signer{did: did, priv: ed25519.NewKeyFromSeed(seed)}, nil
}

type signature struct {
	timestamp string
	nonce     string
	bodySHA   string
	sig       string // standard base64, padded
}

func (s *signer) sign(method, path string, body []byte, now time.Time) (signature, error) {
	sum := sha256.Sum256(body)
	bodySHA := hex.EncodeToString(sum[:]) // lowercase hex, 64 chars

	timestamp := now.UTC().Format("2006-01-02T15:04:05.999999-07:00")

	nonceBytes := make([]byte, 24)
	if _, err := rand.Read(nonceBytes); err != nil {
		return signature{}, fmt.Errorf("generate nonce: %w", err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes) // token_urlsafe(24)

	canonical := canonicalString(method, path, timestamp, nonce, bodySHA)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, []byte(canonical)))

	return signature{timestamp: timestamp, nonce: nonce, bodySHA: bodySHA, sig: sig}, nil
}

func canonicalString(method, path, timestamp, nonce, bodySHA string) string {
	return strings.ToUpper(method) + "\n" + path + "\n" + timestamp + "\n" + nonce + "\n" + bodySHA
}
