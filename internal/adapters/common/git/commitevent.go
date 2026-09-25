package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// patchIDTimeout bounds the diff-tree/patch-id pipeline: the commit already
// exists (this runs from post-commit), so a slow computation on a huge commit
// must never hold the hook open; patch_id is diagnostic and best-effort, so a
// timeout simply omits it (R6).
const patchIDTimeout = 2 * time.Second

// CommitFacts is the structural identity of one commit: everything a commit
// event's metadata (R6) needs and nothing else. No message, no diff, no patch
// body -- only ids and a hash.
type CommitFacts struct {
	SHA     string
	Tree    string
	Parents []string
	// Repo is the canonical remote identity (CanonicalRemote), userinfo
	// stripped; "" when the repo has no usable remote.
	Repo string
	// Branch is "" for a detached HEAD.
	Branch string
	// PatchID is best-effort (git patch-id --stable): "" on a merge commit
	// (more than one parent), a diff-tree/patch-id error, or a timeout.
	PatchID string
}

// ReadCommitFacts reads rev's structural identity. It never returns a partial
// CommitFacts on a caught error: PatchID is the only field allowed to be
// silently absent, because it is diagnostic (R6) and every other field is
// required for R6's metadata contract.
func (g Git) ReadCommitFacts(rev string) (CommitFacts, error) {
	sha, err := g.RevParse(rev)
	if err != nil {
		return CommitFacts{}, fmt.Errorf("read commit facts: %w", err)
	}
	tree, parents, err := g.CommitIdentity(sha)
	if err != nil {
		return CommitFacts{}, fmt.Errorf("read commit facts: %w", err)
	}
	facts := CommitFacts{
		SHA:     sha,
		Tree:    tree,
		Parents: parents,
		Repo:    g.CanonicalRemote(),
	}
	if branch, err := g.currentBranch(); err == nil {
		facts.Branch = branch
	}

	ctx, cancel := context.WithTimeout(context.Background(), patchIDTimeout)
	defer cancel()
	if id, err := g.patchID(ctx, sha, parents); err == nil {
		facts.PatchID = id
	}
	return facts, nil
}

// currentBranch is "" for a detached HEAD: `rev-parse --abbrev-ref HEAD`
// prints the literal string "HEAD" in that state, which is dropped rather
// than stored as a branch name.
func (g Git) currentBranch() (string, error) {
	out, err := g.run("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	b := strings.TrimSpace(string(out))
	if b == "" || b == "HEAD" {
		return "", nil
	}
	return b, nil
}

// patchID is best-effort and returns "" (no error is fatal to the caller) for
// a merge commit, where there is no single parent diff to identify.
func (g Git) patchID(ctx context.Context, sha string, parents []string) (string, error) {
	if len(parents) > 1 {
		return "", nil
	}
	diff, err := g.outputContext(ctx, "diff-tree", "-p", "--root", sha)
	if err != nil {
		return "", fmt.Errorf("diff-tree: %w", err)
	}
	idCmd := g.commandContext(ctx, []string{"patch-id", "--stable"})
	idCmd.Stdin = bytes.NewReader(diff)
	var out, errb bytes.Buffer
	idCmd.Stdout = &out
	idCmd.Stderr = &errb
	if err := idCmd.Run(); err != nil {
		return "", fmt.Errorf("patch-id: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	fields := strings.Fields(out.String())
	if len(fields) == 0 {
		return "", fmt.Errorf("patch-id: empty output")
	}
	return fields[0], nil
}

// CommitSink is the post-commit hook's producer seam: cmd/openbox wires
// routing, identity and delivery behind it (SetCommitSink) so this package
// stays free of devconfig/client imports, the same split RunHook already uses
// for attestation (attesthook.go).
type CommitSink func(CommitFacts, []ResolvedSession)

var commitSink CommitSink

// SetCommitSink installs the sink RunHook's post-commit case calls. nil (the
// zero value; a standalone hook binary with no engine wired) means "nothing to
// call" -- runCommitSink already treats that as a no-op.
func SetCommitSink(fn CommitSink) { commitSink = fn }

// runCommitSink calls the installed sink once per commit, only when there is
// at least one validly-resolved session id to attribute it to; ReadCommitFacts
// runs at most once per commit regardless of how many sessions resolved.
func runCommitSink(g Git, resolved []ResolvedSession, logf func(string, ...any)) {
	if commitSink == nil {
		return // no engine wired (standalone hook binary); nothing to call
	}
	if len(validSessionIDs(sessionIDs(resolved))) == 0 {
		return // an unattributed commit; the trailer already says so too
	}
	facts, err := g.ReadCommitFacts("HEAD")
	if err != nil {
		logf("commit event skipped (commit facts unavailable): %v", err)
		return
	}
	commitSink(facts, resolved)
}

func sessionIDs(resolved []ResolvedSession) []string {
	ids := make([]string, len(resolved))
	for i, rs := range resolved {
		ids[i] = rs.ID
	}
	return ids
}

// commandContext and outputContext mirror command/run (trailer.go) with a
// caller-supplied context, for the one caller (patchID) that must not let a
// slow git subprocess hold the post-commit hook open.
func (g Git) commandContext(ctx context.Context, args []string) *exec.Cmd {
	full := args
	if g.Dir != "" {
		full = append([]string{"-C", g.Dir}, args...)
	}
	cmd := exec.CommandContext(ctx, g.bin(), full...)
	if g.Env != nil {
		cmd.Env = g.Env
	}
	return cmd
}

func (g Git) outputContext(ctx context.Context, args ...string) ([]byte, error) {
	cmd := g.commandContext(ctx, args)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}
