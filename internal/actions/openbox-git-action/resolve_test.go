package gitaction

import (
	"context"
	"os"
	"reflect"
	"testing"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
)

var ctx = context.Background()

func TestResolve_PlainTrailerCommit(t *testing.T) {
	r := newTestRepo(t)
	sha := r.commit(trailerMsg("do work", "sess-A"))

	res, err := r.resolver(nil).Resolve(ctx, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.CommitSHA != sha {
		t.Fatalf("CommitSHA = %s, want %s (real pushed SHA)", res.CommitSHA, sha)
	}
	if got := res.SessionIDs(); !reflect.DeepEqual(got, []string{"sess-A"}) {
		t.Fatalf("sessions = %v, want [sess-A]", got)
	}
	if res.Sessions[0].Source != SourceTrailer {
		t.Fatalf("source = %s, want trailer", res.Sessions[0].Source)
	}
	if res.Status != StatusInferred {
		t.Fatalf("status = %s, want inferred (unverified claim)", res.Status)
	}
	if res.Sessions[0].Verified {
		t.Fatal("claim marked Verified with NoopVerifier")
	}
}

func TestResolve_HumanCommitUnattributed(t *testing.T) {
	r := newTestRepo(t)
	sha := r.commit("manual fix\n") // no trailer anywhere

	res, err := r.resolver(nil).Resolve(ctx, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusUnattributed {
		t.Fatalf("status = %s, want unattributed", res.Status)
	}
	if res.Reason != ReasonNoTrailer {
		t.Fatalf("reason = %s, want no-trailer", res.Reason)
	}
	if len(res.Sessions) != 0 {
		t.Fatalf("sessions = %v, want none (never a wrong guess, INV-6)", res.SessionIDs())
	}
}

func TestResolve_SquashFanInFromTrailerBlock(t *testing.T) {
	r := newTestRepo(t)
	sha := r.commit(trailerMsg("squashed feature", "sess-A", "sess-B", "sess-C"))

	res, err := r.resolver(nil).Resolve(ctx, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"sess-A": true, "sess-B": true, "sess-C": true}
	if got := ids(res.Sessions); !reflect.DeepEqual(got, want) {
		t.Fatalf("fan-in = %v, want %v", got, want)
	}
	for _, s := range res.Sessions {
		if s.Source != SourceTrailer {
			t.Fatalf("%s source = %s, want trailer", s.SessionID, s.Source)
		}
	}
}

func TestResolve_PreInstallSquashRecoveredByBodyScan(t *testing.T) {
	r := newTestRepo(t)
	msg := "squashed before hook\n\n" +
		trailerKey + ": sess-old\n\n" +
		"later prose paragraph that becomes the trailing block\n"
	sha := r.commit(msg)

	block := r.git("show", "-s", "--format=%(trailers:key="+trailerKey+",valueonly)", sha)
	if got := len(nonEmptyLines(block)); got != 0 {
		t.Fatalf("precondition: trailer block should be empty, saw %d lines: %q", got, block)
	}

	res, err := r.resolver(nil).Resolve(ctx, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := res.SessionIDs(); !reflect.DeepEqual(got, []string{"sess-old"}) {
		t.Fatalf("body-scan recovery = %v, want [sess-old]", got)
	}
	if res.Sessions[0].Source != SourceBodyScan {
		t.Fatalf("source = %s, want body-scan (SL6-SCAN)", res.Sessions[0].Source)
	}
	if res.Status != StatusInferred {
		t.Fatalf("status = %s, want inferred", res.Status)
	}
}

func TestResolve_TrailerBlockBeatsBodyScanForSameID(t *testing.T) {
	// It must be counted once, credited to the higher-trust trailer source.
	r := newTestRepo(t)
	msg := "x\n\n" + trailerKey + ": sess-A\n\nmore\n\n" + trailerKey + ": sess-A\n"
	sha := r.commit(msg)

	res, err := r.resolver(nil).Resolve(ctx, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := res.SessionIDs(); !reflect.DeepEqual(got, []string{"sess-A"}) {
		t.Fatalf("sessions = %v, want [sess-A] (deduped)", got)
	}
	if res.Sessions[0].Source != SourceTrailer {
		t.Fatalf("source = %s, want trailer (higher trust wins)", res.Sessions[0].Source)
	}
}

func TestResolve_TrailerBeatsBodyScanAcrossCommits(t *testing.T) {
	// Even though the scope is walked newest-first, the id must be credited
	// SourceTrailer (to the older commit), never mislabeled body-scan.
	r := newTestRepo(t)
	root := r.commit("root\n")
	cTrailer := r.commit(trailerMsg("older, proper trailer", "sess-X"))
	r.commit("newer\n\n" + trailerKey + ": sess-X\n\ntrailing prose\n") // sess-X mid-body

	res, err := r.resolver(nil).Resolve(ctx, r.head(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.SessionIDs(); !reflect.DeepEqual(got, []string{"sess-X"}) {
		t.Fatalf("sessions = %v, want [sess-X] (deduped across commits)", got)
	}
	if res.Sessions[0].Source != SourceTrailer {
		t.Fatalf("source = %s, want trailer (higher trust wins across scope)", res.Sessions[0].Source)
	}
	if res.Sessions[0].Commit != cTrailer {
		t.Fatalf("credited commit = %s, want the trailer-bearing %s", res.Sessions[0].Commit, cTrailer)
	}
}

func TestResolve_MixedRangeRecoversStrippedSibling(t *testing.T) {
	// The sibling's session must be recovered per-commit, not silently dropped
	// because the scope as a whole already had a claim.
	r := newTestRepo(t)
	root := r.commit("root\n")
	r.commit(trailerMsg("A keeps its trailer", "sess-A"))
	b := r.commit("B trailer stripped by a rewrite\n")
	if err := (obgit.Git{Dir: r.dir}).WriteNoteMirror(b, []string{"sess-N"}); err != nil {
		t.Fatal(err)
	}

	res, err := r.resolver(nil).Resolve(ctx, b, root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"sess-A": true, "sess-N": true}
	if got := ids(res.Sessions); !reflect.DeepEqual(got, want) {
		t.Fatalf("mixed range = %v, want %v (sibling not dropped)", got, want)
	}
	var nClaim SessionClaim
	for _, s := range res.Sessions {
		if s.SessionID == "sess-N" {
			nClaim = s
		}
	}
	if nClaim.Source != SourceNote || nClaim.Commit != b {
		t.Fatalf("sess-N = %+v, want note-mirror source on commit %s", nClaim, b)
	}
}

func TestResolve_MaxSessionsCapIsRecorded(t *testing.T) {
	r := newTestRepo(t)
	sha := r.commit(trailerMsg("many", "s1", "s2", "s3", "s4", "s5"))

	res := r.resolver(nil)
	res.MaxSessions = 2
	got, err := res.Resolve(ctx, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 2 {
		t.Fatalf("claims = %d, want 2 (capped)", len(got.Sessions))
	}
	if got.Note == "" || !contains(got.Note, "MaxSessions") {
		t.Fatalf("cap not disclosed in Note: %q", got.Note)
	}
}

func TestResolve_MergeAttributesReachableOriginals(t *testing.T) {
	r := newTestRepo(t)
	r.commit("root\n") // human base on main
	r.git("checkout", "-q", "-b", "feature")
	r.commit(trailerMsg("feat 1", "sess-A"))
	r.commit(trailerMsg("feat 2", "sess-B"))
	r.git("checkout", "-q", "main")
	r.git("merge", "--no-ff", "-m", "Merge feature", "feature")
	merge := r.head()

	res, err := r.resolver(nil).Resolve(ctx, merge, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"sess-A": true, "sess-B": true}
	if got := ids(res.Sessions); !reflect.DeepEqual(got, want) {
		t.Fatalf("merge fan-in = %v, want %v", got, want)
	}
}

func TestResolve_RangeBaseToTarget(t *testing.T) {
	r := newTestRepo(t)
	c1 := r.commit(trailerMsg("c1", "sess-A"))
	r.commit(trailerMsg("c2", "sess-B"))
	c3 := r.commit("c3 human\n")

	res, err := r.resolver(nil).Resolve(ctx, c3, c1)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.SessionIDs(); !reflect.DeepEqual(got, []string{"sess-B"}) {
		t.Fatalf("range sessions = %v, want [sess-B]", got)
	}
	if res.ScopeWalked != 2 {
		t.Fatalf("scope walked = %d, want 2 (c2,c3)", res.ScopeWalked)
	}
}

func TestResolve_ForcePushResolvesRealPushedSHA(t *testing.T) {
	// An amend mints a new SHA with a different trailer; each SHA must resolve to
	// its own content.
	r := newTestRepo(t)
	before := r.commit(trailerMsg("work", "sess-A"))
	r.git("commit", "--amend", "--allow-empty", "--cleanup=verbatim", "-F", writeTmp(t, trailerMsg("work v2", "sess-B")))
	after := r.head()
	if before == after {
		t.Fatal("amend did not change the SHA; test precondition broken")
	}

	resAfter, err := r.resolver(nil).Resolve(ctx, after, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := resAfter.SessionIDs(); !reflect.DeepEqual(got, []string{"sess-B"}) {
		t.Fatalf("pushed SHA %s → %v, want [sess-B]", after, got)
	}
	resBefore, err := r.resolver(nil).Resolve(ctx, before, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := resBefore.SessionIDs(); !reflect.DeepEqual(got, []string{"sess-A"}) {
		t.Fatalf("pre-push SHA %s → %v, want [sess-A]", before, got)
	}
}

func TestResolve_FixupDropIsUnattributedNotWrong(t *testing.T) {
	// The resolver must mark it unattributed, never silently attribute to the
	// wrong (surviving) commit's absent trailer.
	r := newTestRepo(t)
	sha := r.commit("refactor (fixup dropped the trailer)\n")

	res, err := r.resolver(nil).Resolve(ctx, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusUnattributed || res.Reason != ReasonNoTrailer {
		t.Fatalf("status/reason = %s/%s, want unattributed/no-trailer", res.Status, res.Reason)
	}
}

func TestResolve_TrailerStrippedRecoveredFromNoteMirror(t *testing.T) {
	r := newTestRepo(t)
	sha := r.commit("rebased, trailer gone\n")
	if err := (obgit.Git{Dir: r.dir}).WriteNoteMirror(sha, []string{"sess-N"}); err != nil {
		t.Fatal(err)
	}

	res, err := r.resolver(nil).Resolve(ctx, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := res.SessionIDs(); !reflect.DeepEqual(got, []string{"sess-N"}) {
		t.Fatalf("note recovery = %v, want [sess-N]", got)
	}
	if res.Status != StatusInferred || res.Reason != ReasonTrailerStripped {
		t.Fatalf("status/reason = %s/%s, want inferred/trailer-stripped", res.Status, res.Reason)
	}
	if res.Sessions[0].Source != SourceNote {
		t.Fatalf("source = %s, want note-mirror", res.Sessions[0].Source)
	}
}

func TestResolve_RejectsMalformedAndSecretTrailers(t *testing.T) {
	r := newTestRepo(t)
	msg := "x\n\n" +
		trailerKey + ": obx_live_deadbeef\n" +
		trailerKey + ": my great feature\n" +
		trailerKey + ": sess-A\n"
	sha := r.commit(msg)

	res, err := r.resolver(nil).Resolve(ctx, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := res.SessionIDs(); !reflect.DeepEqual(got, []string{"sess-A"}) {
		t.Fatalf("sessions = %v, want [sess-A] (secret + whitespace dropped)", got)
	}
}

func TestResolve_MaxCommitsCapIsRecorded(t *testing.T) {
	r := newTestRepo(t)
	c1 := r.commit(trailerMsg("c1", "sess-A"))
	r.commit(trailerMsg("c2", "sess-B"))
	c3 := r.commit(trailerMsg("c3", "sess-C"))

	res := r.resolver(nil)
	res.MaxCommits = 1
	got, err := res.Resolve(ctx, c3, c1) // range c1..c3 has 2 commits; cap to 1
	if err != nil {
		t.Fatal(err)
	}
	if got.ScopeTotal != 2 || got.ScopeWalked != 1 {
		t.Fatalf("walked/total = %d/%d, want 1/2", got.ScopeWalked, got.ScopeTotal)
	}
	if got.Note == "" {
		t.Fatal("cap not disclosed in Note (silent truncation)")
	}
}

func TestResolve_BadSHAIsPreconditionError(t *testing.T) {
	r := newTestRepo(t)
	r.commit(trailerMsg("x", "sess-A"))
	if _, err := r.resolver(nil).Resolve(ctx, "not-a-real-rev", ""); err == nil {
		t.Fatal("expected an error for an unknown rev (precondition, not a drop)")
	}
	if _, err := r.resolver(nil).Resolve(ctx, "--upload-pack=touch pwned", ""); err == nil {
		t.Fatal("expected an error for a flag-shaped rev")
	}
}

func writeTmp(t *testing.T, s string) string {
	t.Helper()
	f := t.TempDir() + "/m"
	if err := os.WriteFile(f, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}
