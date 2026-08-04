package git

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// TrailerKey is the commit-message trailer that binds a commit to its
// OpenBox session(s). One line per distinct session, like Co-Authored-By.
const TrailerKey = "OpenBox-Session"

// MaxSessionIDLen bounds a stamped id (defense-in-depth, mirrors the
// adapters' maxIdentLen). Claude Code session ids are UUIDs (36 chars); a
// value far larger than any real id is treated as malformed and skipped,
// never stamped.
const MaxSessionIDLen = 512

// Git runs the git binary against a working tree. The zero value uses the
// ambient `git` on PATH and the current directory; tests set Dir to a temp repo.
type Git struct {
	Bin string   // git binary; "" => "git"
	Dir string   // repo working dir passed via `-C`; "" => current dir
	Env []string // full environment for the child; nil => inherit
}

func (g Git) bin() string {
	if g.Bin != "" {
		return g.Bin
	}
	return "git"
}

// run executes `git [-C dir] args...` and returns stdout, mapping a non-zero
// exit to an error that includes stderr (trimmed, secret-free by construction —
// we never pass a secret to git).
func (g Git) run(args ...string) ([]byte, error) {
	full := args
	if g.Dir != "" {
		full = append([]string{"-C", g.Dir}, args...)
	}
	cmd := exec.Command(g.bin(), full...)
	if g.Env != nil {
		cmd.Env = g.Env
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// StampMessageFile stamps an `OpenBox-Session:` trailer for each session
// id onto the commit message at msgFile, idempotently and additively:
//
//   - `--if-missing=add`        first session id creates the trailer block.
//   - `--if-exists=addIfDifferent` a distinct id is appended as a new line
//     (multi-session fan-in); an id already present is not duplicated —
//     this is what makes re-fire and `git commit --amend` safe.
//
// Ids are validated (validateSessionID) before reaching git: empty,
// over-long, multi-line, and secret-shaped values are dropped, never
// stamped (INV-1). If no id survives validation the message is left
// untouched (a human/unattributed commit stays unstamped; the git action
// marks it). Order is preserved.
//
// Squash healing: a squash concatenates each source message, so an
// earlier commit's `OpenBox-Session:` line ends up mid-body — where
// git's trailer parser (and the git action's %(trailers) resolve) does
// not see it. We therefore harvest every OpenBox-Session line from the
// whole message and re-assert it into the trailing trailer block
// (addIfDifferent => no duplication). This makes the multi-session
// fan-in actually resolvable as trailers regardless of who ran the
// squash — even a human with no session of their own still heals the
// agent sessions they squashed together (so the union, not just the
// in-scope current session, is stamped).
func (g Git) StampMessageFile(msgFile string, sessions []string) error {
	if msgFile == "" {
		return fmt.Errorf("stamp: empty message file path")
	}
	data, err := os.ReadFile(msgFile)
	if err != nil {
		return fmt.Errorf("stamp: read %s: %w", msgFile, err)
	}
	// Never interfere: do not stamp a commit message that has no real
	// content (only git comment lines / whitespace). Stamping it would
	// turn a would-be empty commit — which git aborts/rejects — into a
	// junk commit whose sole content is the trailer. Leaving it untouched
	// preserves git's own empty-message handling. (interpret-trailers on
	// an empty body would also produce an unresolvable leading-trailer,
	// not a real message.)
	if !hasCommitContent(data, g.commentChar()) {
		return nil
	}
	// History (harvested from anywhere in the message) first, then the
	// in-scope current session(s) — first occurrence wins on dedupe.
	ids := validSessionIDs(append(scanSessionLines(data), sessions...))
	if len(ids) == 0 {
		return nil // nothing to attribute — leave the message untouched
	}

	args := []string{
		"interpret-trailers",
		"--if-exists=addIfDifferent",
		"--if-missing=add",
	}
	for _, id := range ids {
		// key=value is a single argv element and never begins with '-', so it
		// can never be mistaken for a flag.
		args = append(args, "--trailer", TrailerKey+"="+id)
	}
	args = append(args, "--in-place", msgFile)

	_, err = g.run(args...)
	return err
}

// ReadTrailers returns the deduped set of OpenBox-Session values on a
// message file, in order. Used by tests and by callers that want to
// inspect what is already stamped (the same read the git action performs
// server-side).
func (g Git) ReadTrailers(msgFile string) ([]string, error) {
	out, err := g.run("interpret-trailers", "--parse", msgFile)
	if err != nil {
		return nil, err
	}
	return parseTrailerValues(out), nil
}

// commentChar returns the repo's commit-message comment character
// (core.commentChar, default "#"). "auto" — git picks a char per message — is
// treated as the "#" family (best effort; the common case). Used to decide
// whether a message is effectively empty (hasCommitContent).
func (g Git) commentChar() string {
	out, err := g.run("config", "--get", "core.commentChar")
	if err != nil {
		return "#"
	}
	c := strings.TrimSpace(string(out))
	if c == "" || c == "auto" {
		return "#"
	}
	return c
}

// hasCommitContent reports whether the message has any real content — a line
// that is neither blank nor a git comment. Everything at and below a scissors
// line ("<comment> ... >8 ...", from `--cleanup=scissors` / `commit -v`) is not
// part of the message and is ignored. This mirrors how git decides a message is
// empty (and aborts the commit), so the stamper can avoid resurrecting it.
func hasCommitContent(data []byte, commentChar string) bool {
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if commentChar != "" && strings.HasPrefix(trimmed, commentChar) {
			if strings.Contains(trimmed, ">8") {
				return false // scissors: nothing below counts as the message
			}
			continue
		}
		return true
	}
	return false
}

// scanSessionLines finds `OpenBox-Session: <value>` lines at column 0 anywhere in
// data (comment/indented lines are ignored — a real trailer is unindented and
// uncommented). Validation happens later in validSessionIDs.
func scanSessionLines(data []byte) []string {
	var out []string
	prefix := TrailerKey + ":"
	for _, raw := range strings.Split(string(data), "\n") {
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

// parseTrailerValues extracts `OpenBox-Session` values from `interpret-trailers
// --parse` output (one `Key: value` per line), deduping while preserving order.
func parseTrailerValues(parsed []byte) []string {
	var out []string
	seen := map[string]bool{}
	prefix := TrailerKey + ":"
	for _, line := range strings.Split(string(parsed), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// validSessionIDs filters + dedupes session ids for stamping, preserving
// first occurrence order.
func validSessionIDs(sessions []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range sessions {
		if err := ValidateSessionID(s); err != nil {
			continue
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// ValidateSessionID enforces that only an opaque, single-line, non-secret
// id is ever written into a commit (INV-1 + trailer-injection safety). It
// rejects:
//   - empty / whitespace-only,
//   - anything over MaxSessionIDLen,
//   - a value containing a newline, carriage return, or NUL — which
//     could inject extra trailer lines or split the message,
//   - a value shaped like an OpenBox API key (`obx_` prefix): the
//     trailer is the opaque session id only, never a credential (INV-1).
func ValidateSessionID(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("empty session id")
	}
	if len(id) > MaxSessionIDLen {
		return fmt.Errorf("session id too long (%d > %d)", len(id), MaxSessionIDLen)
	}
	if strings.ContainsAny(id, "\n\r\x00") {
		return fmt.Errorf("session id contains a line break or NUL")
	}
	// A session id is an opaque token (UUID-like) with no internal
	// whitespace. Rejecting spaces/tabs stops a prose body line
	// ("OpenBox-Session: my great feature") from being harvested into a
	// bogus resolvable trailer, and keeps the trailer value a single word.
	if strings.ContainsAny(id, " \t\v\f") {
		return fmt.Errorf("session id contains whitespace")
	}
	if strings.HasPrefix(id, "obx_") {
		return fmt.Errorf("refusing to stamp a secret-shaped value (obx_ prefix)")
	}
	return nil
}

// MaxNoteBytes bounds a single git-notes read. The notes ref is writable by
// anyone who can push it, so an unbounded read lets a hostile note dictate this
// process's memory — and, before the attestation was capped at attach time, put
// arbitrary bytes into a governance record.
const MaxNoteBytes = 1 << 20 // 1 MiB

// runLimited runs git and returns at most maxBytes of stdout, reporting whether
// the output was truncated. Streaming rather than buffering is what makes the
// bound real: g.run reads the whole output before anyone can object.
func (g Git) runLimited(maxBytes int64, args ...string) (out []byte, truncated bool, err error) {
	full := args
	if g.Dir != "" {
		full = append([]string{"-C", g.Dir}, args...)
	}
	cmd := exec.Command(g.bin(), full...)
	if g.Env != nil {
		cmd.Env = g.Env
	}
	stdout, perr := cmd.StdoutPipe()
	if perr != nil {
		return nil, false, perr
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if serr := cmd.Start(); serr != nil {
		return nil, false, serr
	}
	data, _ := io.ReadAll(io.LimitReader(stdout, maxBytes+1))
	if int64(len(data)) > maxBytes {
		truncated = true
		data = data[:maxBytes]
	}
	_, _ = io.Copy(io.Discard, stdout) // drain so Wait cannot deadlock on a full pipe
	if werr := cmd.Wait(); werr != nil {
		return data, truncated, fmt.Errorf("git %s: %w: %s", args[0], werr, strings.TrimSpace(errb.String()))
	}
	return data, truncated, nil
}
