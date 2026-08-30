package git

import "time"

// AttestContext supplies what an attestation needs but this package must not
// resolve for itself: the agent identity, its signing seed, and the policy
// bundle coordinates.
type AttestContext struct {
	DID            string
	PrivateKeyB64  string
	ThreadID       string
	BundlePolicyID string
	BundleSHA256   string
	Adapter        string
}

// AttestContextFunc returns the signing context, or ok=false when this machine
// cannot attest (no credentials configured, or resolution failed).
type AttestContextFunc func() (AttestContext, bool)

var attestContext AttestContextFunc

// SetAttestContext installs the resolver used by the post-commit hook.
func SetAttestContext(fn AttestContextFunc) { attestContext = fn }

func writeAttestation(g Git, sessions []string, logf func(string, ...any)) {
	if attestContext == nil {
		return // no engine wired (standalone hook binary); nothing to say
	}
	if len(validSessionIDs(sessions)) == 0 {
		return // an unattributed commit; honest, and the trailer says so too
	}
	ctx, ok := attestContext()
	if !ok {
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
