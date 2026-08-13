package git

import "time"

// AttestContext supplies what an attestation needs but this package must not
// resolve for itself: the agent identity, its signing seed, and the policy bundle
// coordinates. Keeping secret resolution out of here means the git module never
// touches the secret store — the engine that owns credentials injects a provider.
type AttestContext struct {
	DID            string
	PrivateKeyB64  string
	ThreadID       string
	BundlePolicyID string
	BundleSHA256   string
	Adapter        string
}

// AttestContextFunc returns the signing context, or ok=false when this machine
// cannot attest (no credentials configured, or resolution failed). Returning
// ok=false is a normal outcome: the commit stands and lineage stays inferred.
type AttestContextFunc func() (AttestContext, bool)

// attestContext is the injection point. It stays nil in this module — the CLI
// sets it during init, so the git module keeps no dependency on the secret store
// or on any adapter. Nil means attestation is simply off, which is what a
// standalone git-hook binary built without the engine gets.
var attestContext AttestContextFunc

// SetAttestContext installs the resolver used by the post-commit hook. Called
// once at startup by the engine that owns credentials.
func SetAttestContext(fn AttestContextFunc) { attestContext = fn }

// writeAttestation signs the current commit and stores the attestation as a git
// note. Every failure path is a logged no-op: the commit has already been created,
// so there is nothing to fail, and a missing attestation degrades the deploy
// lineage to "inferred" exactly as before this feature (INV-3).
func writeAttestation(g Git, sessions []string, logf func(string, ...any)) {
	if attestContext == nil {
		return // no engine wired (standalone hook binary); nothing to say
	}
	if len(validSessionIDs(sessions)) == 0 {
		return // an unattributed commit — honest, and the trailer says so too
	}
	ctx, ok := attestContext()
	if !ok {
		// Say so. Silence here is the reason someone ends up asking why their
		// lineage still reads "inferred" with no way to find out.
		logf("attestation skipped: no developer credentials resolved on this machine " +
			"(commit is still attributed by its trailer)")
		return
	}

	sha, err := g.RevParse("HEAD")
	if err != nil {
		logf("attestation skipped (no commit sha): %v", err)
		return
	}
	tree, parents, err := g.CommitIdentity("HEAD")
	if err != nil {
		// Attest the sha alone rather than nothing: a weaker statement that still
		// binds the commit is better than silence, and the verifier treats absent
		// tree/parents as simply not covered.
		logf("attestation: commit identity unavailable, attesting sha only: %v", err)
	}

	att, err := Attest(AttestationInput{
		Repo:           g.CanonicalRemote(),
		CommitSHA:      sha,
		TreeSHA:        tree,
		ParentSHAs:     parents,
		SessionIDs:     sessions,
		ThreadID:       ctx.ThreadID,
		BundlePolicyID: ctx.BundlePolicyID,
		BundleSHA256:   ctx.BundleSHA256,
		Adapter:        ctx.Adapter,
		DID:            ctx.DID,
		PrivateKeyB64:  ctx.PrivateKeyB64,
		Now:            time.Now,
	})
	if err != nil {
		logf("attestation skipped: %v", err)
		return
	}
	if err := g.WriteAttestation(sha, att); err != nil {
		logf("attestation note skipped: %v", err)
	}
}
