package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/backend"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// signPolicy stands in for a signing backend: it builds the payload, signs those
// exact bytes, and returns the block a real response would carry.
func signPolicy(t *testing.T, priv ed25519.PrivateKey, epoch int64, builder string) *backend.SignedPolicy {
	t.Helper()
	payload := decision.SignedPayload{
		PolicyID:      "pol-1",
		AgentID:       "agent-1",
		UpdatedAt:     "2026-07-01T00:00:00Z",
		PolicyEpoch:   epoch,
		ExpiresAt:     "2030-01-01T00:00:00Z",
		VersionHash:   "vh-1",
		PolicyBuilder: json.RawMessage(builder),
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return &backend.SignedPolicy{
		KeyID:        "key-1",
		Algorithm:    decision.AlgorithmEd25519,
		CanonicalB64: base64.StdEncoding.EncodeToString(canonical),
		SigB64:       base64.StdEncoding.EncodeToString(ed25519.Sign(priv, canonical)),
	}
}

const testBuilder = `{"version":1,"rules":[{"id":"r1","decision":"BLOCK","matchMode":"all","conditions":[]}]}`

// A signed policy that verifies is installed, and the accepted epoch is pinned so
// a later rollback can be detected.
func TestSyncBundle_SignedVerifiesAndPinsEpoch(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "policy-bundle.json")
	t.Setenv("OPENBOX_ORG_SIGNING_PUBKEY", base64.StdEncoding.EncodeToString(pub))
	t.Setenv("OPENBOX_CONFIG", filepath.Join(dir, "dev.json"))

	pol := &backend.Policy{
		ID: "pol-1", UpdatedAt: "2026-07-01T00:00:00Z",
		PolicyBuilder: json.RawMessage(testBuilder),
		Signed:        signPolicy(t, priv, 5, testBuilder),
	}
	b, _, err := translateBundle(pol)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	_, note, err := verifySyncedBundle(bundlePath, b)
	if err != nil {
		t.Fatalf("a correctly signed policy must install: %v", err)
	}
	if !strings.Contains(note, "verified") {
		t.Errorf("note should report verification, got %q", note)
	}
	if err := writeBundleFile(bundlePath, b); err != nil {
		t.Fatalf("write: %v", err)
	}
	decision.WriteEpochPin(bundlePath, b.Epoch())
	if got := decision.ReadEpochPin(bundlePath); got != 5 {
		t.Errorf("epoch pin = %d, want 5", got)
	}

	// The installed bundle verifies from disk, and its policy comes from the
	// signed bytes.
	trusted, integrity := decision.VerifyBundleFile(bundlePath, decision.VerifyOptions{
		PublicKey: pub, MinEpoch: decision.ReadEpochPin(bundlePath),
	})
	if integrity != decision.IntegrityVerified || trusted == nil {
		t.Fatalf("round-trip = (%v, %q), want verified", trusted, integrity)
	}
	if trusted.PolicyBuilder == nil || len(trusted.PolicyBuilder.Rules) != 1 {
		t.Errorf("policy not re-derived from the signature: %+v", trusted.PolicyBuilder)
	}
}

// A signature that does not verify aborts the sync and leaves the previous bundle
// in place — installing it and distrusting it later would trade a good policy for
// a bad one.
func TestSyncBundle_TamperedSignatureRefusedAndLastGoodKept(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "policy-bundle.json")
	t.Setenv("OPENBOX_ORG_SIGNING_PUBKEY", base64.StdEncoding.EncodeToString(pub))
	t.Setenv("OPENBOX_CONFIG", filepath.Join(dir, "dev.json"))

	// Install a good bundle first, so there is a last-good to preserve.
	good := &backend.Policy{
		ID: "pol-1", UpdatedAt: "2026-07-01T00:00:00Z",
		PolicyBuilder: json.RawMessage(testBuilder),
		Signed:        signPolicy(t, priv, 5, testBuilder),
	}
	gb, _, _ := translateBundle(good)
	if err := writeBundleFile(bundlePath, gb); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}

	// Now a policy whose signed bytes were altered in transit.
	bad := &backend.Policy{
		ID: "pol-1", UpdatedAt: "2026-07-02T00:00:00Z",
		PolicyBuilder: json.RawMessage(testBuilder),
		Signed:        signPolicy(t, priv, 6, testBuilder),
	}
	raw, _ := base64.StdEncoding.DecodeString(bad.Signed.CanonicalB64)
	raw[len(raw)/2] ^= 0x01
	bad.Signed.CanonicalB64 = base64.StdEncoding.EncodeToString(raw)

	bb, _, _ := translateBundle(bad)
	if _, _, err := verifySyncedBundle(bundlePath, bb); err == nil {
		t.Fatal("a tampered signature must abort the sync")
	}

	after, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the last-good bundle must be left untouched when a sync is refused")
	}
}

// A replayed older-but-genuinely-signed bundle is refused: this is the rollback
// case the epoch pin exists for.
func TestSyncBundle_EpochRollbackRefused(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "policy-bundle.json")
	t.Setenv("OPENBOX_ORG_SIGNING_PUBKEY", base64.StdEncoding.EncodeToString(pub))
	t.Setenv("OPENBOX_CONFIG", filepath.Join(dir, "dev.json"))

	decision.WriteEpochPin(bundlePath, 9) // already accepted epoch 9

	old := &backend.Policy{
		ID: "pol-1", UpdatedAt: "2026-06-01T00:00:00Z",
		PolicyBuilder: json.RawMessage(testBuilder),
		Signed:        signPolicy(t, priv, 4, testBuilder),
	}
	ob, _, _ := translateBundle(old)
	_, _, err := verifySyncedBundle(bundlePath, ob)
	if err == nil || !strings.Contains(err.Error(), string(decision.IntegrityEpochRollback)) {
		t.Fatalf("want an epoch_rollback refusal, got %v", err)
	}
}

// Unsigned stays deployable: a backend that does not sign yet must keep working,
// with a note rather than an error.
func TestSyncBundle_UnsignedAcceptedWithNote(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENBOX_CONFIG", filepath.Join(dir, "dev.json"))
	pol := &backend.Policy{ID: "pol-1", UpdatedAt: "2026-07-01T00:00:00Z",
		PolicyBuilder: json.RawMessage(testBuilder)}
	b, _, _ := translateBundle(pol)
	_, note, err := verifySyncedBundle(filepath.Join(dir, "policy-bundle.json"), b)
	if err != nil {
		t.Fatalf("an unsigned policy must still install: %v", err)
	}
	if !strings.Contains(note, "unsigned") {
		t.Errorf("note should say the policy is unsigned, got %q", note)
	}
}

// Signed but no pinned key: the deployment is incomplete, not the content
// suspect, so it installs with a note explaining how to complete it.
func TestSyncBundle_SignedButNoPinnedKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	dir := t.TempDir()
	t.Setenv("OPENBOX_CONFIG", filepath.Join(dir, "dev.json"))
	t.Setenv("OPENBOX_ORG_SIGNING_PUBKEY", "")
	pol := &backend.Policy{ID: "pol-1", UpdatedAt: "2026-07-01T00:00:00Z",
		PolicyBuilder: json.RawMessage(testBuilder), Signed: signPolicy(t, priv, 5, testBuilder)}
	b, _, _ := translateBundle(pol)
	_, note, err := verifySyncedBundle(filepath.Join(dir, "policy-bundle.json"), b)
	if err != nil {
		t.Fatalf("want install-with-note when no key is pinned, got %v", err)
	}
	if !strings.Contains(note, "no org signing key") {
		t.Errorf("note should explain the missing pin, got %q", note)
	}
}

// TestSyncBundle_UnverifiedEpochIsNotPinned is the E8-S6 regression guard: the
// epoch floor may only advance from a payload whose signature verified.
//
// Bundle.Epoch() decodes the signed payload without checking the signature, so
// pinning from it unconditionally let anyone able to answer the policy fetch set
// the floor. With no org key pinned — the default until org_signing_pubkey is
// populated — a bundle claiming policy_epoch = MaxInt64 pinned that value; every
// genuinely-signed bundle afterwards then verified as IntegrityEpochRollback, the
// decider refused to load any of them, and enforcement was permanently fail-open
// with no way back, since the floor only ever advances.
func TestSyncBundle_UnverifiedEpochIsNotPinned(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "policy-bundle.json")
	t.Setenv("OPENBOX_CONFIG", filepath.Join(dir, "dev.json"))
	t.Setenv("OPENBOX_ORG_SIGNING_PUBKEY", "") // no key pinned → IntegrityNoKey

	hostile := &backend.Policy{
		ID: "pol-1", UpdatedAt: "2026-07-01T00:00:00Z",
		PolicyBuilder: json.RawMessage(testBuilder),
		Signed:        signPolicy(t, priv, math.MaxInt64, testBuilder),
	}
	a, out, _ := syncApp(&fakePolicyReader{pol: hostile})
	if err := a.syncPolicyBundle(context.Background(), "https://backend.example",
		"obx_key_x", "openbox-cli", "agent-1", bundlePath, out); err != nil {
		t.Fatalf("an unverifiable-but-parsable bundle still installs (no_key): %v", err)
	}
	if got := decision.ReadEpochPin(bundlePath); got != 0 {
		t.Fatalf("epoch pin = %d, want 0 — an unverified payload must never set the floor", got)
	}

	// Proof the floor is still usable: a real signed bundle verifies afterwards.
	pub2, priv2, _ := ed25519.GenerateKey(nil)
	t.Setenv("OPENBOX_ORG_SIGNING_PUBKEY", base64.StdEncoding.EncodeToString(pub2))
	real := &backend.Policy{
		ID: "pol-1", UpdatedAt: "2026-07-02T00:00:00Z",
		PolicyBuilder: json.RawMessage(testBuilder),
		Signed:        signPolicy(t, priv2, 7, testBuilder),
	}
	a2, out2, _ := syncApp(&fakePolicyReader{pol: real})
	if err := a2.syncPolicyBundle(context.Background(), "https://backend.example",
		"obx_key_x", "openbox-cli", "agent-1", bundlePath, out2); err != nil {
		t.Fatalf("a genuinely signed bundle must install after a no-key sync: %v", err)
	}
	if got := decision.ReadEpochPin(bundlePath); got != 7 {
		t.Errorf("epoch pin = %d, want 7 (a VERIFIED bundle does advance the floor)", got)
	}
}

// The pin is not advanced for an unsigned bundle either — Epoch() is 0 there, but
// assert it so the gate cannot be relaxed to "any non-zero epoch".
func TestSyncBundle_UnsignedDoesNotPin(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "policy-bundle.json")
	t.Setenv("OPENBOX_CONFIG", filepath.Join(dir, "dev.json"))
	pol := &backend.Policy{ID: "pol-1", UpdatedAt: "2026-07-01T00:00:00Z",
		PolicyBuilder: json.RawMessage(testBuilder)}
	a, out, _ := syncApp(&fakePolicyReader{pol: pol})
	if err := a.syncPolicyBundle(context.Background(), "https://backend.example",
		"obx_key_x", "openbox-cli", "agent-1", bundlePath, out); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := decision.ReadEpochPin(bundlePath); got != 0 {
		t.Errorf("epoch pin = %d, want 0 for an unsigned bundle", got)
	}
}
