package gitaction

import (
	"context"
	"errors"
	"testing"
)

// allowList is a test OwnershipVerifier: it "owns" exactly the listed ids —
// standing in for the deferred backend session-ownership lookup (SL5-SEC-1).
func allowList(owned ...string) OwnershipVerifier {
	set := map[string]bool{}
	for _, id := range owned {
		set[id] = true
	}
	return verifierFunc(func(_ context.Context, id string) (bool, error) {
		return set[id], nil
	})
}

func TestOwnership_VerifiedClaimBecomesAttributed(t *testing.T) {
	r := newTestRepo(t)
	sha := r.commit(trailerMsg("work", "sess-A"))

	res, err := r.resolver(allowList("sess-A")).Resolve(ctx, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusAttributed {
		t.Fatalf("status = %s, want attributed", res.Status)
	}
	if !res.Sessions[0].Verified {
		t.Fatal("owned session not marked Verified")
	}
}

func TestOwnership_PartialVerificationStaysAttributedButFlagsClaims(t *testing.T) {
	// One session is owned by the pusher, one is a forged claim naming a
	// victim's id. The deploy is attributed (a real owner exists) but the
	// forged id is recorded verified=false — never silently trusted.
	r := newTestRepo(t)
	sha := r.commit(trailerMsg("work", "sess-mine", "sess-victim"))

	res, err := r.resolver(allowList("sess-mine")).Resolve(ctx, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusAttributed {
		t.Fatalf("status = %s, want attributed", res.Status)
	}
	byID := map[string]SessionClaim{}
	for _, s := range res.Sessions {
		byID[s.SessionID] = s
	}
	if !byID["sess-mine"].Verified {
		t.Fatal("sess-mine should be Verified")
	}
	if byID["sess-victim"].Verified {
		t.Fatal("forged sess-victim must NOT be Verified (SL5-SEC-1)")
	}
	if byID["sess-victim"].Reason == "" {
		t.Fatal("unverified claim should carry a reason")
	}
}

func TestOwnership_LookupErrorFailsClosedOnAttribution(t *testing.T) {
	// A lookup fault must never over-attribute: the claim stays unverified.
	r := newTestRepo(t)
	sha := r.commit(trailerMsg("work", "sess-A"))
	boom := verifierFunc(func(context.Context, string) (bool, error) {
		return false, errors.New("backend down")
	})

	res, err := r.resolver(boom).Resolve(ctx, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusInferred {
		t.Fatalf("status = %s, want inferred (fail-closed on attribution)", res.Status)
	}
	if res.Sessions[0].Verified {
		t.Fatal("claim marked Verified despite lookup error")
	}
}
