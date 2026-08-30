package gitaction

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
)

const trailerKey = obgit.TrailerKey

// maxMessageBytes a body that exceeds this is truncated and the truncation is
// surfaced (never silent).
const maxMessageBytes = 1 << 20 // 1 MiB

// Repo is a read-only view of a git repository for server-side resolution. All
// git invocations are argv-only (no shell), so a hostile ref name, SHA, or
// trailer value can never inject a flag or a command (mirrors the write side's
// Git.run).
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

// run we never pass a secret to git, so stderr is secret-free by construction.
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

// runLimited unlike run it streams stdout through an io.LimitReader so a
// hostile, arbitrarily large output (e.g. A giant commit body) can never be
// buffered whole into memory (SEC-6-1).
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

func (r Repo) verifyCommit(rev string) (string, error) {
	if strings.TrimSpace(rev) == "" {
		return "", fmt.Errorf("empty rev")
	}
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

func (r Repo) rangeCommits(base, target string, limit int) ([]string, error) {
	out, err := r.run("rev-list", "--max-count", strconv.Itoa(limit), "--end-of-options", base+".."+target)
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(out), nil
}

func (r Repo) mergeIntroduced(merge string, limit int) ([]string, error) {
	out, err := r.run("rev-list", "--max-count", strconv.Itoa(limit), "--end-of-options", merge, "^"+merge+"^1")
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(out), nil
}

func (r Repo) trailerBlockSessions(sha string) (ids []string, truncated bool, err error) {
	out, truncated, err := r.runLimited(maxMessageBytes, "show", "-s",
		"--format=%(trailers:key="+trailerKey+",valueonly,separator=%x0A)",
		"--end-of-options", sha)
	if err != nil {
		return nil, truncated, err
	}
	return nonEmptyLines(out), truncated, nil
}

// bodySessions full-body-scans a commit message for column-0 `OpenBox-
// Session:` lines.
func (r Repo) bodySessions(sha string) (ids []string, truncated bool, err error) {
	out, truncated, err := r.runLimited(maxMessageBytes, "show", "-s", "--format=%B", "--end-of-options", sha)
	if err != nil {
		return nil, truncated, err
	}
	return scanSessionLines(out), truncated, nil
}

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
