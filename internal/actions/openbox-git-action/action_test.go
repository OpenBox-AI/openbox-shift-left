package gitaction

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

type fakeEmitter struct {
	got  []client.DevEvent
	eval client.Evaluation
	err  error
}

func (f *fakeEmitter) Emit(_ context.Context, ev client.DevEvent) (client.Evaluation, error) {
	f.got = append(f.got, ev)
	return f.eval, f.err
}

func fixedNow() time.Time { return time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC) }

func TestAction_EmitsResolvedDeploy(t *testing.T) {
	r := newTestRepo(t)
	sha := r.commit(trailerMsg("ship it", "sess-A"))
	em := &fakeEmitter{eval: client.Evaluation{Verdict: client.VerdictUnknown}}

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

// TestAction_RecordsAdvisoryNeverGatesDeploy proves the Advisory tier on the
// deploy path (story-SL-9): a BLOCK verdict + guardrail hit writes an advisory
// record (would_block=true, category present) yet the deploy still emits and
// Run returns no error (INV-3).
func TestAction_RecordsAdvisoryNeverGatesDeploy(t *testing.T) {
	dir := t.TempDir()
	advPath := filepath.Join(dir, "advisories.jsonl")
	r := newTestRepo(t)
	sha := r.commit(trailerMsg("ship it", "sess-A"))

	em := &fakeEmitter{eval: client.Evaluation{
		Verdict:   client.VerdictBlock,
		RiskScore: 0.88,
		TrustTier: "2",
		Guardrail: &client.GuardrailResult{
			Passed:  false,
			Reasons: []client.GuardrailReason{{Type: "pii", Field: "email", Reason: "Contains PII"}},
		},
	}}
	act := &Action{
		Resolver: r.resolver(allowList("sess-A")),
		Emitter:  em,
		Meta:     DeployMeta{Repo: "o/r", Environment: "production", DeveloperDID: "did:aip:x"},
		Now:      fixedNow,
		Advisory: &Advisory{Path: advPath},
	}
	res, err := act.Run(ctx, sha, "")
	if err != nil {
		t.Fatalf("Run must not error on a BLOCK verdict (INV-3): %v", err)
	}
	if !res.Emitted {
		t.Fatal("deploy must still emit despite the BLOCK verdict")
	}
	if res.Verdict != client.VerdictBlock {
		t.Errorf("Result.Verdict = %q, want BLOCK", res.Verdict)
	}

	raw, err := os.ReadFile(advPath)
	if err != nil {
		t.Fatalf("advisory sink not written: %v", err)
	}
	var rec advisoryRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &rec); err != nil {
		t.Fatalf("advisory record is not valid JSON: %v\n%s", err, raw)
	}
	if rec.Verdict != "BLOCK" || !rec.WouldBlock {
		t.Errorf("record verdict=%q would_block=%t, want BLOCK/true", rec.Verdict, rec.WouldBlock)
	}
	if rec.EventType != "Deploy" || rec.CommitSHA != sha {
		t.Errorf("deploy fields mismapped: type=%q commit=%q (want Deploy/%s)", rec.EventType, rec.CommitSHA, sha)
	}
	if len(rec.GuardrailReasons) != 1 || rec.GuardrailReasons[0].Type != "pii" {
		t.Errorf("guardrail category missing: %+v", rec.GuardrailReasons)
	}
}

func TestAction_FailOpenOnEmitError(t *testing.T) {
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
	r := newTestRepo(t)
	r.commit(trailerMsg("x", "sess-A"))
	act := &Action{Resolver: r.resolver(nil), Emitter: &fakeEmitter{}, Now: fixedNow}
	if _, err := act.Run(ctx, "no-such-rev", ""); err == nil {
		t.Fatal("expected resolve error to surface")
	}
}
