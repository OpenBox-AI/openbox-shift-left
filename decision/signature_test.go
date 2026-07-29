package decision

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// signer stands in for the backend: it builds a payload, serializes it once, and
// signs those exact bytes. Tests carry the same bytes to the verifier, which is
// the contract ADR-0008 pins.
type signer struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newSigner(t *testing.T) signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return signer{pub: pub, priv: priv}
}

func (s signer) sign(t *testing.T, p SignedPayload) *SignedPolicy {
	t.Helper()
	canonical, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return &SignedPolicy{
		KeyID:        "key-1",
		Algorithm:    AlgorithmEd25519,
		CanonicalB64: base64.StdEncoding.EncodeToString(canonical),
		SigB64:       base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, canonical)),
	}
}

// samplePayload is a permissive-but-real builder policy with a far-future expiry.
func samplePayload() SignedPayload {
	return SignedPayload{
		PolicyID:    "pol-1",
		AgentID:     "agent-1",
		UpdatedAt:   "2026-07-01T00:00:00Z",
		PolicyEpoch: 7,
		ExpiresAt:   "2030-01-01T00:00:00Z",
		VersionHash: "vh-1",
		PolicyBuilder: json.RawMessage(`{"rules":[{"id":"r1","match":{"tool_name":"Bash"},` +
			`"decision":"require_approval"}]}`),
	}
}

func opts(s signer) VerifyOptions {
	return VerifyOptions{
		PublicKey: s.pub,
		Now:       func() time.Time { return time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC) },
	}
}

func TestVerifyIntegrity_HappyPath(t *testing.T) {
	s := newSigner(t)
	b := &Bundle{Signed: s.sign(t, samplePayload())}

	trusted, integrity := b.VerifyIntegrity(opts(s))
	if integrity != IntegrityVerified {
		t.Fatalf("integrity = %q, want verified", integrity)
	}
	if !integrity.Trusted() {
		t.Error("a verified bundle must be Trusted")
	}
	if trusted == nil || trusted.PolicyID != "pol-1" {
		t.Fatalf("trusted bundle not derived from the payload: %+v", trusted)
	}
	if trusted.PolicyBuilder == nil || len(trusted.PolicyBuilder.Rules) != 1 {
		t.Fatalf("policy_builder not re-derived from the signed bytes: %+v", trusted.PolicyBuilder)
	}
	if got := b.Epoch(); got != 7 {
		t.Errorf("Epoch() = %d, want 7", got)
	}
}

// The point of the story: policy comes from the signed bytes, so editing the
// convenience fields the file carries cannot change a decision.
func TestVerifyIntegrity_TamperingWithFileFieldsIsInert(t *testing.T) {
	s := newSigner(t)
	b := &Bundle{
		// An attacker rewrites the fields a naive evaluator would read, leaving
		// the signed block untouched.
		PolicyID: "pol-attacker",
		PolicyBuilder: &PolicyBuilderConfig{
			Rules: []PolicyBuilderRule{{ID: "evil", Decision: "allow"}},
		},
		DefaultDecision: "allow",
		Signed:          s.sign(t, samplePayload()),
	}

	trusted, integrity := b.VerifyIntegrity(opts(s))
	if integrity != IntegrityVerified {
		t.Fatalf("integrity = %q, want verified (the signed block is intact)", integrity)
	}
	if trusted.PolicyID != "pol-1" {
		t.Errorf("policy id came from the file, not the signature: %q", trusted.PolicyID)
	}
	if len(trusted.PolicyBuilder.Rules) != 1 || trusted.PolicyBuilder.Rules[0].ID != "r1" {
		t.Errorf("the attacker's rules survived verification: %+v", trusted.PolicyBuilder.Rules)
	}
}

// Editing the signed bytes themselves is what a signature is for.
func TestVerifyIntegrity_AlteredSignedBytesFail(t *testing.T) {
	s := newSigner(t)
	sig := s.sign(t, samplePayload())

	// Flip one byte of the signed payload and re-encode.
	canonical, err := base64.StdEncoding.DecodeString(sig.CanonicalB64)
	if err != nil {
		t.Fatal(err)
	}
	altered := append([]byte(nil), canonical...)
	altered[len(altered)/2] ^= 0x01
	sig.CanonicalB64 = base64.StdEncoding.EncodeToString(altered)

	if trusted, integrity := (&Bundle{Signed: sig}).VerifyIntegrity(opts(s)); integrity != IntegrityBadSignature || trusted != nil {
		t.Errorf("altered payload = (%v, %q), want (nil, bad_signature)", trusted, integrity)
	}
}

// A bundle signed by a key this client does not trust must not verify, or the
// pinning would be decorative.
func TestVerifyIntegrity_ForeignKeyFails(t *testing.T) {
	real, attacker := newSigner(t), newSigner(t)
	b := &Bundle{Signed: attacker.sign(t, samplePayload())}
	if _, integrity := b.VerifyIntegrity(opts(real)); integrity != IntegrityBadSignature {
		t.Errorf("integrity = %q, want bad_signature for a foreign key", integrity)
	}
}

func TestVerifyIntegrity_EpochRollbackRefused(t *testing.T) {
	s := newSigner(t)
	p := samplePayload()
	p.PolicyEpoch = 3 // genuinely signed, but superseded
	b := &Bundle{Signed: s.sign(t, p)}

	o := opts(s)
	o.MinEpoch = 7 // this client has already accepted epoch 7
	trusted, integrity := b.VerifyIntegrity(o)
	if integrity != IntegrityEpochRollback || trusted != nil {
		t.Errorf("replayed old bundle = (%v, %q), want (nil, epoch_rollback)", trusted, integrity)
	}

	// The same epoch is fine — a re-sync of the current policy is not a rollback.
	o.MinEpoch = 3
	if _, integrity := b.VerifyIntegrity(o); integrity != IntegrityVerified {
		t.Errorf("re-accepting the same epoch = %q, want verified", integrity)
	}
}

func TestVerifyIntegrity_ExpiryRefused(t *testing.T) {
	s := newSigner(t)
	p := samplePayload()
	p.ExpiresAt = "2026-07-01T00:00:00Z" // before the pinned clock
	if _, integrity := (&Bundle{Signed: s.sign(t, p)}).VerifyIntegrity(opts(s)); integrity != IntegrityExpired {
		t.Errorf("integrity = %q, want expired", integrity)
	}
}

// An unsigned bundle is the compatibility path: it reports Unsigned so the
// caller can keep today's behaviour, and it is never Trusted.
func TestVerifyIntegrity_UnsignedIsCompatButNotTrusted(t *testing.T) {
	b := &Bundle{Version: "no-policy"}
	trusted, integrity := b.VerifyIntegrity(VerifyOptions{})
	if integrity != IntegrityUnsigned {
		t.Errorf("integrity = %q, want unsigned", integrity)
	}
	if integrity.Trusted() {
		t.Error("an unsigned bundle must not be Trusted")
	}
	if trusted != nil {
		t.Error("unsigned verification must not yield a trusted bundle")
	}
	if b.Epoch() != 0 {
		t.Errorf("Epoch() = %d, want 0 for an unsigned bundle", b.Epoch())
	}
}

// A signature that cannot be checked is reported as an incomplete deployment,
// not as suspect content — the two need different operator responses.
func TestVerifyIntegrity_NoPinnedKey(t *testing.T) {
	s := newSigner(t)
	b := &Bundle{Signed: s.sign(t, samplePayload())}
	if _, integrity := b.VerifyIntegrity(VerifyOptions{}); integrity != IntegrityNoKey {
		t.Errorf("integrity = %q, want no_key", integrity)
	}
}

// Anything undecodable is untrusted, never treated as absent — otherwise
// corrupting the block would be a way to fall back to the unsigned path.
func TestVerifyIntegrity_MalformedIsUntrustedNotAbsent(t *testing.T) {
	s := newSigner(t)
	good := s.sign(t, samplePayload())

	cases := map[string]*SignedPolicy{
		"bad canonical base64": {Algorithm: AlgorithmEd25519, CanonicalB64: "!!!", SigB64: good.SigB64},
		"empty canonical":      {Algorithm: AlgorithmEd25519, CanonicalB64: "", SigB64: good.SigB64},
		"bad sig base64":       {Algorithm: AlgorithmEd25519, CanonicalB64: good.CanonicalB64, SigB64: "!!!"},
		"short sig":            {Algorithm: AlgorithmEd25519, CanonicalB64: good.CanonicalB64, SigB64: base64.StdEncoding.EncodeToString([]byte("short"))},
		"downgraded algorithm": {Algorithm: "none", CanonicalB64: good.CanonicalB64, SigB64: good.SigB64},
	}
	for name, sig := range cases {
		if _, integrity := (&Bundle{Signed: sig}).VerifyIntegrity(opts(s)); integrity != IntegrityMalformed {
			t.Errorf("%s: integrity = %q, want malformed", name, integrity)
		}
	}
}

// A signed payload cannot express a deny-by-default policy: the builder config
// carries only first-match rules, and no match means allow. So the fail-open
// default invariant survives signing by construction rather than by a check —
// worth pinning, because a future payload field that added a default would
// silently reopen it.
func TestVerifyIntegrity_SignedPayloadCannotDenyByDefault(t *testing.T) {
	s := newSigner(t)
	p := samplePayload()
	p.PolicyBuilder = json.RawMessage(`{"version":1,"rules":[]}`)
	trusted, integrity := (&Bundle{Signed: s.sign(t, p)}).VerifyIntegrity(opts(s))
	if integrity != IntegrityVerified {
		t.Fatalf("integrity = %q, want verified", integrity)
	}
	if trusted.DefaultDecision != "" {
		t.Errorf("a signed payload must not be able to set a blocking default, got %q",
			trusted.DefaultDecision)
	}
}

// A policy_builder that is not valid JSON is untrusted rather than silently
// dropped — otherwise corrupting it would be a way to get an empty policy.
func TestVerifyIntegrity_UnparsablePolicyBuilderIsMalformed(t *testing.T) {
	s := newSigner(t)
	p := samplePayload()
	p.PolicyBuilder = json.RawMessage(`{"rules": "not-an-array"}`)
	if trusted, integrity := (&Bundle{Signed: s.sign(t, p)}).VerifyIntegrity(opts(s)); integrity != IntegrityMalformed || trusted != nil {
		t.Errorf("unparsable builder = (%v, %q), want (nil, malformed)", trusted, integrity)
	}
}
