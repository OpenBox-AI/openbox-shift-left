package decision

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// signedBundleFile writes a bundle that carries a real signature block plus its own
// (identical) policy fields — what `dev sync` installs from a signing backend.
func signedBundleFile(t *testing.T, s signer) string {
	t.Helper()
	return writeBundle(t, &Bundle{
		Version:         "v1",
		PolicyID:        "pol-1",
		UpdatedAt:       "2026-07-01T00:00:00Z",
		DefaultDecision: "allow",
		Rules: []Rule{{
			ID:       "rmrf",
			Match:    RuleMatch{ToolName: "Bash", AttributeContains: map[string]string{"command": "rm -rf /"}},
			Decision: "block",
			Reason:   "destructive",
		}},
		Signed: s.sign(t, SignedPayload{
			PolicyID:    "pol-1",
			AgentID:     "agent-1",
			UpdatedAt:   "2026-07-01T00:00:00Z",
			PolicyEpoch: 7,
			ExpiresAt:   "2030-01-01T00:00:00Z",
			VersionHash: "vh-1",
			PolicyBuilder: json.RawMessage(`{"rules":[{"id":"rmrf","match":{"tool_name":"Bash"},` +
				`"decision":"block","reason":"destructive"}]}`),
		}),
	})
}

// TestVerifyBundleFile_NoKeyIsUnverifiableNotUntrusted is the E8-S6 regression
// guard for the install/load asymmetry.
//
// `dev sync` accepts a signed bundle when no org key is pinned (the deployment is
// incomplete, not the content suspect) and reports success. VerifyBundleFile used
// to return nil for that outcome, so the decider had nothing to load: the day a
// backend started signing, every install without org_signing_pubkey — i.e. all of
// them, since the key is new — silently stopped enforcing while `dev sync` still
// said it had synced.
func TestVerifyBundleFile_NoKeyIsUnverifiableNotUntrusted(t *testing.T) {
	s := newSigner(t)
	path := signedBundleFile(t, s)

	// No PublicKey in the options: the client has not pinned a key.
	b, integrity := VerifyBundleFile(path, VerifyOptions{})
	if integrity != IntegrityNoKey {
		t.Fatalf("integrity = %q, want no_key", integrity)
	}
	if b == nil {
		t.Fatal("no_key must return the file's own bundle so policy still loads — " +
			"returning nil is what silently disabled enforcement")
	}
	if len(b.Rules) != 1 {
		t.Errorf("expected the file's own policy, got %+v", b.Rules)
	}
	// Unverifiable is still not trusted: posture must be able to say so.
	if integrity.Trusted() {
		t.Error("no_key must not report Trusted() — nothing was verified")
	}

	// With the key pinned, the same file verifies and the policy is re-derived
	// from the signed bytes.
	if _, got := VerifyBundleFile(path, opts(s)); got != IntegrityVerified {
		t.Errorf("with the key pinned, integrity = %q, want verified", got)
	}
}

// A signature that actually FAILS still loads nothing — the fix must not blur
// "cannot check" into "checked and bad".
func TestVerifyBundleFile_BadSignatureStillLoadsNothing(t *testing.T) {
	s, foreign := newSigner(t), newSigner(t)
	path := signedBundleFile(t, s)
	b, integrity := VerifyBundleFile(path, opts(foreign))
	if integrity != IntegrityBadSignature {
		t.Fatalf("integrity = %q, want bad_signature", integrity)
	}
	if b != nil {
		t.Error("a bundle that failed verification must never be returned")
	}
}

// TestInProcess_NoKeySignedBundleStillEnforces is the same guard one level up: the
// decider reaches a REAL verdict from a signed-but-unverifiable bundle instead of
// degrading to cold-start fail-open (which, under the opt-in fail-closed policy,
// would have denied every tool call instead).
func TestInProcess_NoKeySignedBundleStillEnforces(t *testing.T) {
	s := newSigner(t)
	path := signedBundleFile(t, s)

	d := NewInProcessDecider(InProcessConfig{BundlePath: path}) // no SigningPubKey
	if got := d.Integrity(); got != IntegrityNoKey {
		t.Fatalf("integrity = %q, want no_key", got)
	}
	dec := d.Decide(context.Background(), toolCall("Bash", client.ToolShell,
		map[string]any{"command": "rm -rf / now"}))
	if dec.FailOpen {
		t.Fatalf("a signed-but-unpinned bundle must still be enforced, got fail-open (%s)", dec.Source)
	}
	if dec.Evaluation.Verdict != client.VerdictBlock {
		t.Errorf("verdict = %q, want BLOCK from the loaded policy", dec.Evaluation.Verdict)
	}
}

// A bundle whose signature fails still leaves the decider at fail-open, and the
// recorded integrity says why.
func TestInProcess_BadSignatureFailsOpen(t *testing.T) {
	s, foreign := newSigner(t), newSigner(t)
	path := signedBundleFile(t, s)

	d := NewInProcessDecider(InProcessConfig{BundlePath: path, SigningPubKey: foreign.pub})
	if got := d.Integrity(); got != IntegrityBadSignature {
		t.Fatalf("integrity = %q, want bad_signature", got)
	}
	dec := d.Decide(context.Background(), toolCall("Bash", client.ToolShell,
		map[string]any{"command": "rm -rf / now"}))
	if !dec.FailOpen {
		t.Error("a bundle that failed verification must not be evaluated")
	}
}

// The epoch pin must not advance from an unverifiable bundle, even though it now
// loads: the payload carrying that epoch was never authenticated.
func TestInProcess_NoKeyDoesNotPinEpoch(t *testing.T) {
	s := newSigner(t)
	path := signedBundleFile(t, s)

	_ = NewInProcessDecider(InProcessConfig{BundlePath: path})
	if got := ReadEpochPin(path); got != 0 {
		t.Errorf("epoch pin = %d, want 0 — no_key must not advance the floor", got)
	}
	// A verified load does advance it.
	_ = NewInProcessDecider(InProcessConfig{BundlePath: path, SigningPubKey: s.pub})
	if got := ReadEpochPin(path); got != 7 {
		t.Errorf("epoch pin = %d, want 7 after a verified load", got)
	}
}
