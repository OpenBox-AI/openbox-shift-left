package git

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// TrailerKey is the commit-message trailer that binds a commit to its OpenBox
// session(s).
const TrailerKey = "OpenBox-Session"

// MaxSessionIDLen bounds a stamped id (defense-in-depth, mirrors the adapters'
// maxIdentLen). Claude Code session ids are UUIDs (36 chars); a value far
// larger than any real id is treated as malformed and skipped, never stamped.
const MaxSessionIDLen = 512

// Git runs the git binary against a working tree.
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

// StampMessageFile stamps an `OpenBox-Session:` trailer for each session id
// onto the commit message at msgFile, idempotently and additively:
//   - `--if-missing=add` first session id creates the trailer block.
//   - `--if-exists=addIfDifferent` a distinct id is appended as a new line
//     (multi-session fan-in); an id already present is not duplicated; this is
//     what makes re-fire and `git commit --amend` safe.
func (g Git) StampMessageFile(msgFile string, sessions []string) error {
	if msgFile == "" {
		return fmt.Errorf("stamp: empty message file path")
	}
	data, err := os.ReadFile(msgFile)
	if err != nil {
		return fmt.Errorf("stamp: read %s: %w", msgFile, err)
	}
	if !hasCommitContent(data, g.commentChar()) {
		return nil
	}
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
		args = append(args, "--trailer", TrailerKey+"="+id)
	}
	args = append(args, "--in-place", msgFile)

	_, err = g.run(args...)
	return err
}

// ReadTrailers returns the deduped set of OpenBox-Session values on a message
// file, in order.
func (g Git) ReadTrailers(msgFile string) ([]string, error) {
	out, err := g.run("interpret-trailers", "--parse", msgFile)
	if err != nil {
		return nil, err
	}
	return parseTrailerValues(out), nil
}

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

// ValidateSessionID enforces that only an opaque, single-line, non-secret id
// is ever written into a commit (INV-1 + trailer-injection safety).
//   - Empty / whitespace-only,
//   - Anything over MaxSessionIDLen,
//   - A value containing a newline, carriage return, or NUL; which could
//     inject extra trailer lines or split the message,
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
	if strings.ContainsAny(id, " \t\v\f") {
		return fmt.Errorf("session id contains whitespace")
	}
	if strings.HasPrefix(id, "obx_") {
		return fmt.Errorf("refusing to stamp a secret-shaped value (obx_ prefix)")
	}
	return nil
}

// MaxNoteBytes bounds a single git-notes read.
const MaxNoteBytes = 1 << 20 // 1 MiB

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
