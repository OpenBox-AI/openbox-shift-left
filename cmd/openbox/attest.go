package main

import (
	"os"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
)

// attestContext resolves what a commit attestation needs to be signed (E8-S10).
//
// It runs in a post-commit hook, so it does touch the secret store — acceptable
// here and only here: the commit already exists, nothing is blocking on it, and
// this is off every tool-call hot path. A resolution failure returns ok=false and
// the commit stays unattested, which is exactly the pre-E8 behaviour rather than
// an error a developer has to deal with.
func attestContext() (obgit.AttestContext, bool) {
	creds, err := devconfig.ResolveCredentials()
	if err != nil || creds.DID == "" || creds.PrivateKeyB64 == "" {
		return obgit.AttestContext{}, false
	}

	ctx := obgit.AttestContext{
		DID:           creds.DID,
		PrivateKeyB64: creds.PrivateKeyB64,
		Adapter:       "openbox-cli",
		// The ambient Codex thread id, so a commit made in a forked thread can
		// still be joined to the root session's events (E8-S4).
		ThreadID: os.Getenv(obgit.EnvCodexThreadID),
	}

	// The bundle pin and hash that used to ride here are gone with the local
	// bundle (ADR-0017). They recorded which policy was in force, which the
	// endpoint could only answer while it was the decider; the control plane is
	// the decider now and holds its own record of the policy it applied, per
	// call. Carrying a stale local id would be attesting to something this
	// process no longer knows.
	return ctx, true
}
