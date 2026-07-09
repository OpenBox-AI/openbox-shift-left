package gitaction

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	obgit "github.com/openbox-ai/openbox-shift-left/adapters/common/git"
)

// trailerKey is the commit-message trailer that binds a commit to its OpenBox
// session(s). Reused from the SL-5 write side so both sides agree by
// construction (there is exactly one authoritative key).
const trailerKey = obgit.TrailerKey

// maxMessageBytes bounds how much of a single commit message the resolver reads
// (SEC-6-1). Real messages are tiny; a hostile committer could otherwise author
// a multi-hundred-MB body to OOM the resolver and break CI (INV-3). A body that
// exceeds this is truncated and the truncation is surfaced (never silent).
const maxMessageBytes = 1 << 20 // 1 MiB

// Repo is a read-only view of a git repository for server-side resolution. All
// git invocations are argv-only (no shell), so a hostile ref name, SHA, or
// trailer value can never inject a flag or a command (mirrors SL-5's Git.run).
type Repo struct {
	Bin string // git binary; "" => "git"
	Dir string // repo working dir passed via `-C`; "" => current dir
}

func (r Repo) bin() string {
	if r.Bin != "" {
		return r.Bin
	}
	return "git"
}

// run executes `git [-C dir] args...` and returns stdout, mapping a non-zero
// exit to an error including stderr. We never pass a secret to git, so stderr is
// secret-free by construction.
func (r Repo) run(args ...string) (string, error) {
	full := args
	if r.Dir != "" {
		full = append([]string{"-C", r.Dir}, args...)
	}
	cmd := exec.Command(r.bin(), full...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// runLimited runs git and returns at most maxBytes of stdout, reporting whether
// the output was truncated. Unlike run it streams stdout through an
// io.LimitReader so a hostile, arbitrarily large output (e.g. a giant commit
// body) can never be buffered whole into memory (SEC-6-1). The remainder is
// drained (constant memory) so the child never blocks on a full pipe.
func (r Repo) runLimited(maxBytes int64, args ...string) (out string, truncated bool, err error) {
	full := args
	if r.Dir != "" {
		full = append([]string{"-C", r.Dir}, args...)
	}
	cmd := exec.Command(r.bin(), full...)
	stdout, perr := cmd.StdoutPipe()
	if perr != nil {
		return "", false, perr
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if serr := cmd.Start(); serr != nil {
		return "", false, serr
	}
	data, _ := io.ReadAll(io.LimitReader(stdout, maxBytes+1))
	if int64(len(data)) > maxBytes {
		truncated = true
		data = data[:maxBytes]
	}
	_, _ = io.Copy(io.Discard, stdout) // drain so Wait doesn't deadlock on a full pipe
	if werr := cmd.Wait(); werr != nil {
		return string(data), truncated, fmt.Errorf("git %s: %w: %s", args[0], werr, strings.TrimSpace(errb.String()))
	}
	return string(data), truncated, nil
}

// verifyCommit resolves rev to a full 40-hex commit SHA and confirms it names a
// real commit object. This is the INV-6 "real pushed SHA" gate: the caller
// passes whatever the CI system reports as pushed (a ref, short SHA, or full
// SHA) and gets back the canonical commit id, or an error if it is not a commit.
func (r Repo) verifyCommit(rev string) (string, error) {
	if strings.TrimSpace(rev) == "" {
		return "", fmt.Errorf("empty rev")
	}
	// `--verify --end-of-options <rev>^{commit}` peels tags/annotated refs to a
	// commit and fails cleanly on a non-commit or unknown rev. --end-of-options
	// stops a rev that starts with '-' from being read as a flag.
	out, err := r.run("rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve commit %q: %w", rev, err)
	}
	sha := strings.TrimSpace(out)
	if sha == "" {
		return "", fmt.Errorf("rev %q does not resolve to a commit", rev)
	}
	return sha, nil
}

// parents returns the parent SHAs of a commit (empty for a root commit).
func (r Repo) parents(sha string) ([]string, error) {
	out, err := r.run("rev-list", "--parents", "-n", "1", "--end-of-options", sha)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) <= 1 {
		return nil, nil // just the commit itself, no parents
	}
	return fields[1:], nil
}

// rangeCommits lists the commits in base..target (target-side, excluding base
// and its ancestors), newest first, reading at most limit commits (SEC-6-1: a
// huge range must not be buffered whole). The caller passes maxCommits+1 so it
// can detect and disclose a cap.
func (r Repo) rangeCommits(base, target string, limit int) ([]string, error) {
	out, err := r.run("rev-list", "--max-count", strconv.Itoa(limit), "--end-of-options", base+".."+target)
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(out), nil
}

// mergeIntroduced lists the commits a merge commit brings in that were not
// already on its first-parent line: `rev-list <merge> ^<merge>^1`. The merge
// commit itself is included (it is reachable from itself but not from its first
// parent). This is the "reachable originals" set for a merge (the story). The
// `^rev` exclude form is used (not `--not`) so ordering after --end-of-options
// is unambiguous. Bounded by limit (== maxCommits+1) like rangeCommits.
func (r Repo) mergeIntroduced(merge string, limit int) ([]string, error) {
	out, err := r.run("rev-list", "--max-count", strconv.Itoa(limit), "--end-of-options", merge, "^"+merge+"^1")
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(out), nil
}

// trailerBlockSessions returns the OpenBox-Session values in a commit's trailing
// trailer block — the authoritative read (S3 R7). This is byte-for-byte the
// command SL-5's own tests assert against, so the write and read sides agree.
// The read is size-bounded (SEC-6-1); truncated reports if the block was cut.
func (r Repo) trailerBlockSessions(sha string) (ids []string, truncated bool, err error) {
	out, truncated, err := r.runLimited(maxMessageBytes, "show", "-s",
		"--format=%(trailers:key="+trailerKey+",valueonly,separator=%x0A)",
		"--end-of-options", sha)
	if err != nil {
		return nil, truncated, err
	}
	return nonEmptyLines(out), truncated, nil
}

// bodySessions full-body-scans a commit message for column-0 `OpenBox-Session:`
// lines (SL6-SCAN). It recovers ids left mid-body by a squash performed before
// SL-5's hook (its healing) was in place — where the trailer parser cannot see
// them. Comment/indented lines are ignored (a real trailer is unindented). The
// read is size-bounded (SEC-6-1); truncated reports if the body was cut.
func (r Repo) bodySessions(sha string) (ids []string, truncated bool, err error) {
	out, truncated, err := r.runLimited(maxMessageBytes, "show", "-s", "--format=%B", "--end-of-options", sha)
	if err != nil {
		return nil, truncated, err
	}
	return scanSessionLines(out), truncated, nil
}

// scanSessionLines finds `OpenBox-Session: <value>` lines at column 0 anywhere
// in the message (mirrors SL-5's scanSessionLines; kept local because the read
// side is a separate module and the write-side helper is unexported).
func scanSessionLines(msg string) []string {
	var out []string
	prefix := trailerKey + ":"
	for _, raw := range strings.Split(msg, "\n") {
		line := strings.TrimRight(raw, "\r")
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		if v := strings.TrimSpace(strings.TrimPrefix(line, prefix)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if v := strings.TrimSpace(line); v != "" {
			out = append(out, v)
		}
	}
	return out
}
