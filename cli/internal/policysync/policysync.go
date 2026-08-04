// Package policysync performs the control-plane half of ADR-0005 policy
// distribution: fetch this agent's current policy, translate it into a local
// decision bundle, verify it, and write it.
//
// It lived in package main, which meant ~215 lines of domain logic — fetch,
// translate, verify-before-replace, atomic write, epoch pin — were reachable
// only from the command wiring and testable only through it.
package policysync

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"context"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/backend"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// PolicyReader is the control-plane read this package needs. backend.Client
// implements it; tests inject a fake.
type PolicyReader interface {
	GetCurrentPolicy(ctx context.Context, agentID string) (*backend.Policy, error)
}

// syncPolicyBundle performs the fetch → translate → write → clear-markers flow,
// shared by `dev sync` and the `init` last step. It returns a mapped error on
// failure (the caller decides exit code / warn); it never prints a secret.
func Run(ctx context.Context, reader PolicyReader, agentID, bundlePath string, out io.Writer) error {
	pol, err := reader.GetCurrentPolicy(ctx, agentID)
	if err != nil {
		return mapPolicyReadError(err)
	}

	bundle, note, err := translateBundle(pol)
	if err != nil {
		return err
	}
	// Verify before replacing the last-good bundle (E8-S6). A bundle that fails
	// verification is refused outright rather than written and then distrusted at
	// load: writing it would discard a policy that DID verify in favour of one
	// that did not, which is the wrong direction on every axis.
	integrity, integrityNote, err := verifySyncedBundle(bundlePath, bundle)
	if err != nil {
		return err
	}
	if err := writeBundleFile(bundlePath, bundle); err != nil {
		return fmt.Errorf("write policy bundle: %w", err)
	}
	// Pin the epoch ONLY from a verified signature — the same gate
	// NewInProcessDecider applies. Bundle.Epoch reads the signed payload without
	// checking the signature — VerifiedEpoch is the accessor that will not —
	// so pinning from the raw value would let anyone
	// who can answer the policy fetch set the floor: a bundle claiming
	// policy_epoch = MaxInt64 makes every genuinely-signed bundle afterwards verify
	// as IntegrityEpochRollback, the decider then refuses to load any of them, and
	// enforcement is fail-open from then on. The floor only ever advances
	// (WriteEpochPin), so there is no way back short of deleting the pin file.
	// Reachable today via the no-key path, which is the default until
	// org_signing_pubkey is populated.
	if epoch, ok := bundle.VerifiedEpoch(integrity); ok && epoch > 0 {
		decision.WriteEpochPin(bundlePath, epoch)
	}
	// A fresh, re-pinned bundle clears any fail-closed staleness block so
	// the PreToolUse enforce gate proceeds again.
	_ = hookflow.ClearAllStaleMarkers()

	// Non-secret summary only: the policy id + pin, never the rego or org key (INV-1).
	if pol == nil {
		fmt.Fprintf(out, "Synced policy bundle → %s (no current policy for this agent — allow/no-policy bundle).\n", bundlePath)
	} else {
		fmt.Fprintf(out, "Synced policy bundle → %s (policy %s, updated_at %s).\n", bundlePath, pol.ID, orUnset(pol.UpdatedAt))
	}
	if note != "" {
		fmt.Fprintln(out, note)
	}
	if integrityNote != "" {
		fmt.Fprintln(out, integrityNote)
	}
	return nil
}

// verifySyncedBundle checks a freshly fetched bundle's signature before it is
// allowed to replace the last-good one. It returns the verification outcome plus a
// non-secret note to print, or an error that aborts the sync.
//
// The outcome is returned rather than collapsed to ok/not-ok because the caller
// needs it: only IntegrityVerified may advance the epoch pin, since every other
// outcome means the payload carrying that epoch was never authenticated.
//
// An unsigned bundle is accepted with a note, not an error: backends that do not
// sign yet must keep working, and pretending otherwise would make the feature
// undeployable. A bundle that carries a signature and fails to verify is a
// different matter — that is either tampering in transit or a key mismatch, and
// continuing would mean trusting content we just proved untrustworthy.
func verifySyncedBundle(bundlePath string, b *decision.Bundle) (decision.Integrity, string, error) {
	if b == nil || b.Signed == nil {
		return decision.IntegrityUnsigned, "note: this policy is unsigned — the local bundle cannot be integrity-checked " +
			"(a local edit would not be detectable). Signing is served by newer backends (E8-S6).", nil
	}
	pubKeyB64, keyID := devconfig.ResolveOrgSigningKey()
	pub := decision.DecodePublicKey(pubKeyB64)
	if pub == nil {
		return decision.IntegrityNoKey, "note: this policy is signed but no org signing key is pinned, so the signature " +
			"could not be checked and its epoch is not pinned. It is installed and will be ENFORCED UNVERIFIED " +
			"(a local edit would not be detectable). Pin org_signing_pubkey in dev.json to enable verification.", nil
	}
	_, integrity := b.VerifyIntegrity(decision.VerifyOptions{
		PublicKey: pub,
		MinEpoch:  decision.ReadEpochPin(bundlePath),
	})
	if integrity == decision.IntegrityVerified {
		return integrity, fmt.Sprintf("Policy signature verified (key %s, epoch %d).", orUnset(keyID), b.Epoch()), nil
	}
	return integrity, "", fmt.Errorf("refusing to install policy bundle: signature check failed (%s); "+
		"the previous bundle is unchanged", integrity)
}

// translateBundle maps a fetched *backend.Policy into a *decision.Bundle + an
// optional non-secret note to print. nil policy → an empty allow bundle;
// config.policy_builder → a builder bundle; raw rego with no builder → a
// fail-open-local bundle + a warning (ADR-0005 §Decision-2).
func translateBundle(pol *backend.Policy) (*decision.Bundle, string, error) {
	if pol == nil {
		return &decision.Bundle{Version: "no-policy"}, "", nil
	}
	pin := pol.ID + "@" + pol.UpdatedAt
	signed := signatureBlock(pol)
	if len(pol.PolicyBuilder) > 0 {
		var cfg decision.PolicyBuilderConfig
		if err := json.Unmarshal(pol.PolicyBuilder, &cfg); err != nil {
			return nil, "", fmt.Errorf("parse policy_builder config: %w", err)
		}
		return &decision.Bundle{
			Version:       pin,
			PolicyID:      pol.ID,
			UpdatedAt:     pol.UpdatedAt,
			PolicyBuilder: &cfg,
			Signed:        signed,
		}, "", nil
	}
	if pol.HasRawRego {
		note := "warning: this policy is hand-written raw rego with no builder config — it cannot be evaluated locally " +
			"and the decider will serve it fail-open (allow) locally; enforcement for it relies on the async /evaluate audit (ADR-0005)."
		return &decision.Bundle{
			Version:            pin,
			PolicyID:           pol.ID,
			UpdatedAt:          pol.UpdatedAt,
			RawRegoUnlocalized: true,
			Signed:             signed,
		}, note, nil
	}
	// A policy with neither builder config nor rego → treat as no-op allow, pinned.
	return &decision.Bundle{Version: pin, PolicyID: pol.ID, UpdatedAt: pol.UpdatedAt, Signed: signed}, "", nil
}

// signatureBlock converts the backend's signature block for the bundle file. The
// signed bytes are carried verbatim: re-serializing them here would risk not
// reproducing what the backend signed, and the whole point is that the decider
// verifies the signer's own bytes.
func signatureBlock(pol *backend.Policy) *decision.SignedPolicy {
	if pol == nil || pol.Signed == nil {
		return nil
	}
	return &decision.SignedPolicy{
		KeyID:        pol.Signed.KeyID,
		Algorithm:    pol.Signed.Algorithm,
		CanonicalB64: pol.Signed.CanonicalB64,
		SigB64:       pol.Signed.SigB64,
	}
}

// writeBundleFile marshals the bundle and writes it 0600 (owner-only),
// creating the parent dir 0700. It round-trips through decision.ParseBundle
// first so a malformed/deny-by-default bundle is rejected before it
// replaces the last-good file (never write a bundle the daemon would refuse
// to load).
//
// The write is atomic: a temp file in the same dir (so rename is atomic on
// one filesystem) written 0600, then os.Rename over the target. A crash
// mid-write can never leave the daemon (or the session-start staleness
// read) a truncated/half-parsed bundle — it sees either the old file or the
// whole new one.
func writeBundleFile(path string, b *decision.Bundle) error {
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	if _, err := decision.ParseBundle(raw); err != nil {
		return fmt.Errorf("refusing to write an invalid bundle: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".policy-bundle-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// mapPolicyReadError turns a control-plane read failure into an actionable,
// secret-free hint. It surfaces the exact 4xx cause without echoing any
// credential.
func mapPolicyReadError(err error) error {
	var apiErr *backend.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 401:
			return fmt.Errorf("policy read rejected (HTTP 401): the control-plane credential is invalid or expired — check OPENBOX_CONTROL_TOKEN")
		case 403:
			return fmt.Errorf("policy read forbidden (HTTP 403): the credential lacks the read:agent_policy permission for this org")
		case 404:
			return fmt.Errorf("policy read not found (HTTP 404): the agent id may be wrong for this org — re-check `openbox init`")
		default:
			return fmt.Errorf("policy read failed (HTTP %d)", apiErr.StatusCode)
		}
	}
	return fmt.Errorf("policy read failed: %w", err)
}

// orUnset renders an empty value as "(unset)" so a diagnostic never shows a
// bare blank where a coordinate should be.
func orUnset(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}
