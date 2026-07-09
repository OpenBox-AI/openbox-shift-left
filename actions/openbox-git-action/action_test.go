package gitaction

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

type fakeEmitter struct {
	got     []client.DevEvent
	verdict client.Verdict
	err     error
}

func (f *fakeEmitter) Emit(_ context.Context, ev client.DevEvent) (client.Verdict, error) {
	f.got = append(f.got, ev)
	return f.verdict, f.err
}

func fixedNow() time.Time { return time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC) }

func TestAction_EmitsResolvedDeploy(t *testing.T) {
	r := newTestRepo(t)
	sha := r.commit(trailerMsg("ship it", "sess-A"))
	em := &fakeEmitter{verdict: client.VerdictUnknown}

	act := &Action{
		Resolver: r.resolver(allowList("sess-A")),
		Emitter:  em,
		Meta:     DeployMeta{Repo: "o/r", Environment: "production", DeveloperDID: "did:aip:x"},
		Now:      fixedNow,
	}
	res, err := act.Run(ctx, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Emitted || len(em.got) != 1 {
		t.Fatalf("expected exactly one emit, emitted=%t got=%d", res.Emitted, len(em.got))
	}
	if res.Resolution.Status != StatusAttributed {
		t.Fatalf("status = %s, want attributed", res.Resolution.Status)
	}
	if em.got[0].Metadata["commit_sha"] != sha {
		t.Fatalf("emitted commit_sha = %v, want %s", em.got[0].Metadata["commit_sha"], sha)
	}
}

func TestAction_FailOpenOnEmitError(t *testing.T) {
	// A transport-ish failure must never break the deploy (INV-3). Run returns
	// no error and the full Resolution is still available.
	r := newTestRepo(t)
	sha := r.commit(trailerMsg("x", "sess-A"))
	em := &fakeEmitter{err: errors.New("network down")}

	act := &Action{Resolver: r.resolver(nil), Emitter: em, Now: fixedNow}
	res, err := act.Run(ctx, sha, "")
	if err != nil {
		t.Fatalf("Run returned error on emit failure (should fail-open): %v", err)
	}
	if res.Emitted {
		t.Fatal("Emitted=true despite emit error")
	}
	if res.Resolution.CommitSHA != sha {
		t.Fatal("resolution lost on emit failure")
	}
}

func TestAction_DryRunDoesNotEmit(t *testing.T) {
	r := newTestRepo(t)
	sha := r.commit(trailerMsg("x", "sess-A"))
	act := &Action{Resolver: r.resolver(nil), Emitter: nil, Now: fixedNow} // no emitter
	res, err := act.Run(ctx, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Emitted {
		t.Fatal("Emitted=true with a nil Emitter")
	}
	if res.Event.EventID == "" {
		t.Fatal("dry-run should still build the event")
	}
}

func TestAction_ResolveErrorIsSurfaced(t *testing.T) {
	// A bad SHA is a precondition fault, NOT a fail-open drop — it must surface.
	r := newTestRepo(t)
	r.commit(trailerMsg("x", "sess-A"))
	act := &Action{Resolver: r.resolver(nil), Emitter: &fakeEmitter{}, Now: fixedNow}
	if _, err := act.Run(ctx, "no-such-rev", ""); err == nil {
		t.Fatal("expected resolve error to surface")
	}
}
