package gitaction

import "context"

// OwnershipVerifier answers the SL5-SEC-1 question for every resolved session
// id: does this id name a session OWNED BY the authenticated pusher? A commit
// trailer is an untrusted claim — anyone can name any session id — so a claim is
// only promoted to a proven (Verified) attribution when the verifier says the
// pusher owns it. This mirrors how SL-3 cross-binds the DID: identity is checked
// against the authenticated principal, never taken on faith.
//
// Contract: OwnsSession returns (true, nil) only when ownership is POSITIVELY
// established. It returns (false, nil) when the pusher provably does not own the
// session AND when ownership cannot be determined — either way the id is not
// promoted. An error is for a transport/lookup fault the caller may log; the
// resolver treats (false, err) the same as unverified (fail-closed on
// attribution: never over-attribute on a lookup failure).
type OwnershipVerifier interface {
	OwnsSession(ctx context.Context, sessionID string) (bool, error)
}

// NoopVerifier verifies nothing: every id stays an unverified claim. It is the
// Phase-1 default because the session-ownership read API is external/deferred
// (EXT-lineage / FR-7). With it, a well-formed deploy resolves as Inferred with
// all claims flagged verified=false — honest about what is proven. Swapping in
// a real verifier promotes owned sessions to Attributed with no change here.
type NoopVerifier struct{}

// OwnsSession always reports "not verified" (ownership undetermined).
func (NoopVerifier) OwnsSession(context.Context, string) (bool, error) { return false, nil }

// verifierFunc adapts a function to OwnershipVerifier (for tests and simple
// allow-list verifiers).
type verifierFunc func(ctx context.Context, sessionID string) (bool, error)

func (f verifierFunc) OwnsSession(ctx context.Context, id string) (bool, error) { return f(ctx, id) }
