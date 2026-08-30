package gitaction

import "context"

// OwnershipVerifier answers, for every resolved session id: does this id name
// a session owned by the authenticated pusher? Identity is checked against the
// authenticated principal, never taken on faith.
type OwnershipVerifier interface {
	OwnsSession(ctx context.Context, sessionID string) (bool, error)
}

// NoopVerifier verifies nothing: every id stays an unverified claim.
type NoopVerifier struct{}

// OwnsSession always reports "not verified" (ownership undetermined).
func (NoopVerifier) OwnsSession(context.Context, string) (bool, error) { return false, nil }

type verifierFunc func(ctx context.Context, sessionID string) (bool, error)

func (f verifierFunc) OwnsSession(ctx context.Context, id string) (bool, error) { return f(ctx, id) }
