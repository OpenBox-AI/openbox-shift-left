package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"

	claudecode "github.com/openbox-ai/openbox-shift-left/adapters/claude-code"
	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	obgit "github.com/openbox-ai/openbox-shift-left/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// attestContext resolves what a commit attestation needs to be signed (E8-S10).
//
// It runs in a post-commit hook, so it does touch the secret store — acceptable
// here and only here: the commit already exists, nothing is blocking on it, and
// this is off every tool-call hot path. A resolution failure returns ok=false and
// the commit stays unattested, which is exactly the pre-E8 behaviour rather than
// an error a developer has to deal with.
func attestContext() (obgit.AttestContext, bool) {
	creds, err := devconfig.ResolveCredentials(devconfig.OSSecretLookup)
	if err != nil || creds.DID == "" || creds.SeedB64 == "" {
		return obgit.AttestContext{}, false
	}

	ctx := obgit.AttestContext{
		DID:     creds.DID,
		SeedB64: creds.SeedB64,
		Adapter: "openbox-cli",
		// The ambient Codex thread id, so a commit made in a forked thread can
		// still be joined to the root session's events (E8-S4).
		ThreadID: os.Getenv(obgit.EnvCodexThreadID),
	}

	// Record which policy was in force. This is what makes the attestation worth
	// more than provenance alone: a deploy gate can ask whether the code was
	// written under current policy, not just who wrote it.
	bundlePath := claudecode.ResolveBundlePath()
	if raw, err := os.ReadFile(bundlePath); err == nil {
		sum := sha256.Sum256(raw)
		ctx.BundleSHA256 = hex.EncodeToString(sum[:])
	}
	if b, err := decodeBundlePin(bundlePath); err == nil {
		ctx.BundlePolicyID = b
	}
	return ctx, true
}

// decodeBundlePin reads just the policy id from the local bundle. It deliberately
// does not verify the signature: the attestation records which policy was in
// force, and the separate bundle_integrity posture field (E8-S6) records whether
// that policy was trustworthy. Conflating them would let an unverifiable bundle
// erase the provenance record too.
func decodeBundlePin(path string) (string, error) {
	b, err := decision.LoadBundleFile(path)
	if err != nil || b == nil {
		return "", err
	}
	return b.PolicyID, nil
}
