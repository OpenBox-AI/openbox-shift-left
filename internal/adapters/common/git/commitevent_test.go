package git

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadCommitFacts_RootCommit a commit with no parent still gets a patch
// id: `git diff-tree -p --root` is exactly what makes a root commit's diff
// (against the empty tree) available to `patch-id`. An empty commit has no
// diff at all, so this writes a real file: patch-id needs content to hash.
func TestReadCommitFacts_RootCommit(t *testing.T) {
	r := newRepo(t)
	if err := os.WriteFile(filepath.Join(r.dir, "f.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.git(nil, "add", "f.txt")
	r.git(nil, "commit", "-m", "root")

	facts, err := r.g.ReadCommitFacts("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Parents) != 0 {
		t.Fatalf("root commit Parents = %v, want empty", facts.Parents)
	}
	if facts.PatchID == "" {
		t.Fatalf("root commit PatchID is empty, want a computed patch id")
	}
	if facts.SHA == "" || facts.Tree == "" {
		t.Fatalf("facts = %+v, want SHA and Tree populated", facts)
	}
}

// TestReadCommitFacts_MergeCommitHasNoPatchID a merge commit has more than one
// parent, so there is no single-parent diff to identify (R6): PatchID must
// stay empty rather than describe only one side of the merge.
func TestReadCommitFacts_MergeCommitHasNoPatchID(t *testing.T) {
	r := newRepo(t)
	r.git(nil, "commit", "--allow-empty", "-m", "base")
	r.git(nil, "checkout", "-q", "-b", "side")
	r.git(nil, "commit", "--allow-empty", "-m", "side work")
	r.git(nil, "checkout", "-q", "main")
	r.git(nil, "commit", "--allow-empty", "-m", "main work")
	r.git(nil, "merge", "--no-ff", "-m", "merge side", "side")

	facts, err := r.g.ReadCommitFacts("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Parents) < 2 {
		t.Fatalf("merge commit Parents = %v, want at least 2", facts.Parents)
	}
	if facts.PatchID != "" {
		t.Fatalf("merge commit PatchID = %q, want empty (no single-parent diff)", facts.PatchID)
	}
}

// TestReadCommitFacts_DetachedHEADHasNoBranch `rev-parse --abbrev-ref HEAD`
// prints the literal "HEAD" in a detached state; ReadCommitFacts must drop it
// rather than store it as a branch name.
func TestReadCommitFacts_DetachedHEADHasNoBranch(t *testing.T) {
	r := newRepo(t)
	r.git(nil, "commit", "--allow-empty", "-m", "work")
	sha := r.g.mustRevParse(t, "HEAD")
	r.git(nil, "checkout", "-q", sha)

	facts, err := r.g.ReadCommitFacts("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if facts.Branch != "" {
		t.Fatalf("detached HEAD Branch = %q, want empty", facts.Branch)
	}
}

// TestReadCommitFacts_RemoteUserinfoStripped Repo reuses CanonicalRemote,
// which already strips embedded credentials; this proves ReadCommitFacts
// carries that through rather than reading the raw remote URL.
func TestReadCommitFacts_RemoteUserinfoStripped(t *testing.T) {
	r := newRepo(t)
	r.git(nil, "commit", "--allow-empty", "-m", "work")
	r.git(nil, "remote", "add", "origin", "https://user:secret@github.com/acme/app.git")

	facts, err := r.g.ReadCommitFacts("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if facts.Repo != "github.com/acme/app" {
		t.Fatalf("Repo = %q, want userinfo stripped", facts.Repo)
	}
}

// mustRevParse is a small test-only helper: RevParse itself is exercised
// directly by ReadCommitFacts, so a failure here would already fail the
// surrounding test's own git commands.
func (g Git) mustRevParse(t *testing.T, rev string) string {
	t.Helper()
	sha, err := g.RevParse(rev)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}
