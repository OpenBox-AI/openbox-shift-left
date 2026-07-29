package decision

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Policy-bundle integrity (E8-S6, ADR-0008).
//
// The local bundle is a JSON file in the developer's own config directory. Any
// process running as that user can edit a rule, roll the file back to a
// permissive older version, or keep a stale copy indefinitely, and nothing
// downstream could tell (report SL-02). Signing closes that: the decider trusts
// policy content only when a signature over it verifies against a pinned public
// key, the epoch has not gone backwards, and it has not expired.
//
// The load-bearing detail is WHERE the trusted policy comes from. Verifying a
// signature over a payload and then evaluating the surrounding file's fields
// would prove nothing — an attacker would simply edit the fields the evaluator
// actually reads and leave the signed payload alone. So verification re-derives
// the policy from the signed bytes themselves and returns that; the bundle's own
// PolicyBuilder/Rules fields become a debugging view whose contents cannot
// affect a decision. That is what makes a local edit detectable end to end.
//
// Compatibility: a bundle with no signature block verifies as Unsigned, and the
// caller keeps today's behaviour. A backend that does not sign yet is therefore
// not a regression, it is simply not an improvement.

// Integrity is the outcome of checking a bundle's signature block.
type Integrity string

const (
	// IntegrityUnsigned — no signature block. Either the backend does not sign
	// yet, or someone removed the block; the two are indistinguishable from
	// here, which is exactly why a managed deployment eventually has to require
	// signing rather than merely prefer it.
	IntegrityUnsigned Integrity = "unsigned"
	// IntegrityVerified — signature, epoch and expiry all check out, and the
	// evaluated policy was re-derived from the signed bytes.
	IntegrityVerified Integrity = "verified"
	// IntegrityNoKey — a signature is present but no public key was pinned, so
	// it cannot be checked. Distinct from a bad signature: the deployment is
	// incomplete rather than the content suspect.
	IntegrityNoKey Integrity = "no_key"
	// IntegrityBadSignature — the signature does not verify. The content was
	// altered, or it was signed by a key this client does not trust.
	IntegrityBadSignature Integrity = "bad_signature"
	// IntegrityExpired — the signature verifies but the bundle is past its
	// expiry, so the org's policy has had time to move on without this client
	// noticing.
	IntegrityExpired Integrity = "expired"
	// IntegrityEpochRollback — the signature verifies but the epoch is older
	// than one already seen, i.e. a genuinely-signed but superseded bundle was
	// replayed to restore a more permissive policy.
	IntegrityEpochRollback Integrity = "epoch_rollback"
	// IntegrityMalformed — the signature block or the signed payload could not
	// be decoded. Treated as untrusted, never as absent.
	IntegrityMalformed Integrity = "malformed"
)

// Trusted reports whether policy from this bundle may be evaluated as
// authenticated org policy. Only a full verification qualifies; every other
// outcome, including Unsigned, does not.
//
// It deliberately says nothing about what to DO with an untrusted bundle —
// whether to fall back to fail-open or to deny high-risk tool classes is a
// posture decision the caller owns (OD-E8-3), not a property of the crypto.
func (i Integrity) Trusted() bool { return i == IntegrityVerified }

// SignedPolicy is the signature block a signing backend attaches to a policy.
type SignedPolicy struct {
	KeyID     string `json:"key_id"`
	Algorithm string `json:"algorithm"` // "Ed25519"
	// CanonicalB64 is the exact byte sequence that was signed, base64
	// (std, padded). It is carried verbatim rather than re-serialized from
	// parsed fields, because any re-serialization risks not reproducing the
	// signer's bytes — key order, number formatting, unicode escaping — and a
	// verifier that reconstructs its own input is a verifier that eventually
	// disagrees with the signer.
	CanonicalB64 string `json:"canonical_b64"`
	SigB64       string `json:"sig_b64"`
}

// SignedPayload is the structure encoded in CanonicalB64. The backend builds it,
// signs its serialization, and sends both. Field names and semantics are pinned
// in ADR-0008 because signer and verifier live in different repositories.
type SignedPayload struct {
	PolicyID    string `json:"policy_id"`
	AgentID     string `json:"agent_id"`
	UpdatedAt   string `json:"updated_at"`
	PolicyEpoch int64  `json:"policy_epoch"`
	ExpiresAt   string `json:"expires_at"` // RFC3339
	VersionHash string `json:"version_hash"`
	// PolicyBuilder is the authoritative policy. What the evaluator runs comes
	// from here and nowhere else.
	PolicyBuilder json.RawMessage `json:"policy_builder,omitempty"`
	// RawRegoUnlocalized marks a hand-written-rego policy that cannot be
	// evaluated locally, signed so the client cannot be lied to about it.
	RawRegoUnlocalized bool `json:"raw_rego_unlocalized,omitempty"`
}

// VerifyOptions carries what verification needs from the caller. Keeping the
// key, the epoch floor and the clock as inputs leaves this package free of I/O
// and of any notion of where trust is configured (INV-3b).
type VerifyOptions struct {
	// PublicKey is the org's pinned signing key. Empty ⇒ IntegrityNoKey.
	PublicKey ed25519.PublicKey
	// MinEpoch is the highest epoch this client has already accepted. A payload
	// at or above it is fine; below it is a rollback.
	MinEpoch int64
	// Now defaults to time.Now.
	Now func() time.Time
}

func (o VerifyOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// VerifyIntegrity checks the bundle's signature block and, on success, returns a
// bundle whose policy was re-derived from the signed bytes.
//
// The returned bundle is nil for every outcome except IntegrityVerified: there
// is no partially-trusted policy, so there is nothing to hand back. The caller
// keeps its previous bundle (or none) and applies its own posture.
func (b *Bundle) VerifyIntegrity(opts VerifyOptions) (*Bundle, Integrity) {
	if b == nil {
		return nil, IntegrityMalformed
	}
	if b.Signed == nil {
		return nil, IntegrityUnsigned
	}
	sig := b.Signed

	if sig.Algorithm != "" && sig.Algorithm != AlgorithmEd25519 {
		return nil, IntegrityMalformed
	}
	canonical, err := base64.StdEncoding.DecodeString(sig.CanonicalB64)
	if err != nil || len(canonical) == 0 {
		return nil, IntegrityMalformed
	}
	rawSig, err := base64.StdEncoding.DecodeString(sig.SigB64)
	if err != nil || len(rawSig) != ed25519.SignatureSize {
		return nil, IntegrityMalformed
	}

	// No key pinned: report that the deployment is incomplete rather than
	// implying the content is suspect.
	if len(opts.PublicKey) != ed25519.PublicKeySize {
		return nil, IntegrityNoKey
	}
	if !ed25519.Verify(opts.PublicKey, canonical, rawSig) {
		return nil, IntegrityBadSignature
	}

	// Only now is the payload worth reading.
	var payload SignedPayload
	if err := json.Unmarshal(canonical, &payload); err != nil {
		return nil, IntegrityMalformed
	}

	// Rollback before expiry: a replayed old bundle is a deliberate act, and
	// saying so is more useful than reporting whichever check happens to fire.
	if payload.PolicyEpoch < opts.MinEpoch {
		return nil, IntegrityEpochRollback
	}
	if payload.ExpiresAt != "" {
		expiry, err := time.Parse(time.RFC3339, payload.ExpiresAt)
		if err != nil {
			return nil, IntegrityMalformed
		}
		if opts.now().After(expiry) {
			return nil, IntegrityExpired
		}
	}

	trusted, err := payload.bundle()
	if err != nil {
		return nil, IntegrityMalformed
	}
	// Carry the signature block through so a caller can record the epoch it
	// just accepted, but note the policy content came from the payload.
	trusted.Signed = sig
	return trusted, IntegrityVerified
}

// AlgorithmEd25519 is the only signature algorithm accepted. Pinning it here
// rather than trusting the field means a bundle cannot ask to be verified with
// something weaker.
const AlgorithmEd25519 = "Ed25519"

// bundle builds the evaluatable bundle from the signed payload alone. This is
// the whole point of the exercise: the policy the evaluator runs is derived
// here, from bytes covered by the signature, so edits to the surrounding file
// cannot reach a decision.
func (p SignedPayload) bundle() (*Bundle, error) {
	out := &Bundle{
		Version:            p.PolicyID + "@" + p.UpdatedAt,
		PolicyID:           p.PolicyID,
		UpdatedAt:          p.UpdatedAt,
		RawRegoUnlocalized: p.RawRegoUnlocalized,
	}
	if len(p.PolicyBuilder) > 0 {
		var cfg PolicyBuilderConfig
		if err := json.Unmarshal(p.PolicyBuilder, &cfg); err != nil {
			return nil, fmt.Errorf("parse signed policy_builder: %w", err)
		}
		out.PolicyBuilder = &cfg
	}
	if err := out.validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// Epoch returns the signed epoch, or 0 when the bundle is unsigned. Callers
// persist it as the floor for the next load, which is what makes rollback
// detectable across restarts.
func (b *Bundle) Epoch() int64 {
	if b == nil || b.Signed == nil {
		return 0
	}
	var payload SignedPayload
	canonical, err := base64.StdEncoding.DecodeString(b.Signed.CanonicalB64)
	if err != nil {
		return 0
	}
	if json.Unmarshal(canonical, &payload) != nil {
		return 0
	}
	return payload.PolicyEpoch
}

// ── loading and pinning ────────────────────────────────────────────────────

// VerifyBundleFile loads a bundle from disk and verifies its integrity in one
// step, returning the bundle whose policy may be evaluated plus the outcome.
//
// The returned bundle is the re-derived, trusted one for IntegrityVerified, the
// file's own bundle for IntegrityUnsigned (the compatibility path), and nil for
// every failure — so a caller can never accidentally evaluate policy that did
// not verify. A missing or unparsable file yields (nil, IntegrityMalformed).
func VerifyBundleFile(path string, opts VerifyOptions) (*Bundle, Integrity) {
	b, err := LoadBundleFile(path)
	if err != nil || b == nil {
		return nil, IntegrityMalformed
	}
	trusted, integrity := b.VerifyIntegrity(opts)
	switch integrity {
	case IntegrityVerified:
		return trusted, integrity
	case IntegrityUnsigned:
		return b, integrity
	default:
		return nil, integrity
	}
}

// EpochPinPath is where the highest accepted policy epoch is remembered, beside
// the bundle it describes.
func EpochPinPath(bundlePath string) string { return bundlePath + ".epoch" }

// ReadEpochPin returns the highest epoch this client has accepted, or 0 when
// there is no pin yet. A missing or unreadable pin reads as 0 — the permissive
// direction, because refusing to load policy over a corrupt bookkeeping file
// would turn a local disk problem into a governance outage. The cost of that
// choice is that deleting the pin file re-enables a rollback, which is why the
// pin is a supporting control and the signature is the primary one.
func ReadEpochPin(bundlePath string) int64 {
	raw, err := os.ReadFile(EpochPinPath(bundlePath))
	if err != nil {
		return 0
	}
	epoch, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || epoch < 0 {
		return 0
	}
	return epoch
}

// WriteEpochPin records an accepted epoch, never lowering it: the floor may only
// advance, so a caller that verifies an older-but-valid bundle cannot walk the
// floor back down. Best-effort — failing to persist only means a later rollback
// goes undetected, which must not break the current session.
func WriteEpochPin(bundlePath string, epoch int64) {
	if epoch <= 0 || epoch <= ReadEpochPin(bundlePath) {
		return
	}
	p := EpochPinPath(bundlePath)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(p, []byte(strconv.FormatInt(epoch, 10)), 0o600)
}

// DecodePublicKey parses a base64 raw Ed25519 public key as pinned in config.
// An empty or malformed value yields nil, which verification reports as
// IntegrityNoKey — an unconfigured client is never silently trusted.
func DecodePublicKey(b64 string) ed25519.PublicKey {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(raw)
}
