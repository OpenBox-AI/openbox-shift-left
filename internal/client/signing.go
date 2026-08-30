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
// the docstring pins to openbox-core agent.go's verifier). The client MUST
// match these exactly or core rejects the signature.
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
	// (INV-5). It is not part of the AIP canonical string (only
	// METHOD\nPATH\nTIMESTAMP\nNONCE\nBODY_SHA256), so adding it never
	// affects signature verification. Inert until core dedupes on it — core
	// accepts and ignores unknown headers today; the body still carries the
	// same id in metadata.event_id.
	headerIdempotencyKey = "Idempotency-Key"
)

// sdkVersion is advertised via X-OpenBox-SDK-Version / User-Agent. core reads
// sdk_version from this header (never from the body — MAPPING.md §6). Tracks the
// reference SDK's wire contract, tagged so developer-runtime traffic is
// distinguishable from the Python agent-runtime SDK.
const sdkVersion = "openbox-shift-left/0.1.0"

// signer holds an agent's AIP Ed25519 identity. The seed is never logged or
// exposed (INV-1); only the derived signatures leave this type.
type signer struct {
	did  string
	priv ed25519.PrivateKey
}

// newSigner builds a signer from a base64-encoded raw 32-byte Ed25519 seed —
// the exact form openbox-backend returns as identity.privateKey and the CLI
// stores. Mirrors config.py:190-203: std-base64 decode, require 32 bytes,
// ed25519.NewKeyFromSeed (== Ed25519PrivateKey.from_private_bytes).
func newSigner(did, seedB64 string) (*signer, error) {
	// A malformed DID makes core reject (or fail to key) every signed request,
	// which — under fail-open — would silently drop all telemetry. Catch it at
	// construction. core requires did:aip:<uuid>; we check the scheme prefix.
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

// signature is the set of AIP headers (minus Authorization) for one request.
type signature struct {
	timestamp string
	nonce     string
	bodySHA   string
	sig       string // standard base64, padded
}

// sign builds the AIP signature over (method, path, body) exactly as
// request_signing.py:84-90 does:
//
//	bodySHA   = lowercasehex(sha256(body))
//	timestamp = RFC3339 with a +00:00 UTC offset (the SAME string signed + sent)
//	nonce     = base64url-nopad(24 random bytes)
//	canonical = UPPER(METHOD)\nPATH\nTIMESTAMP\nNONCE\nBODY_SHA256  (LF, no trailing)
//	sig       = base64.Std(ed25519.Sign(priv, utf8(canonical)))     (no pre-hash)
//
// now is injected so tests can pin the timestamp; production passes time.Now.
func (s *signer) sign(method, path string, body []byte, now time.Time) (signature, error) {
	sum := sha256.Sum256(body)
	bodySHA := hex.EncodeToString(sum[:]) // lowercase hex, 64 chars

	// Match Python datetime.now(timezone.utc).isoformat(): a +00:00 offset (not
	// "Z"). The identical string is both signed and sent, so core reconstructs
	// the canonical string from this exact header value.
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

// canonicalString assembles the 5-component AIP canonical string
// (request_signing.py:87 / core agent.go:93). Method is upper-cased (matching
// core's strings.ToUpper(method)); components are joined by a single LF with no
// trailing newline.
func canonicalString(method, path, timestamp, nonce, bodySHA string) string {
	return strings.ToUpper(method) + "\n" + path + "\n" + timestamp + "\n" + nonce + "\n" + bodySHA
}
