// Package prompt is the interactive input layer for `openbox auth`: plain
// lines, masked secrets, and yes/no confirmation.
//
// WHAT MASKING BUYS, precisely, because over-reading it would be a security
// mistake: it keeps a pasted credential out of terminal scrollback, screen
// shares, recorded sessions and tmux buffers, and — because a credential is
// typed rather than passed as a flag — off argv and out of shell history
// (INV-1). It does NOT protect the value at rest. The next thing that happens
// to it is being written to a plaintext file, by design. A masked prompt is
// not evidence of a protected secret.
//
// The interface exists because term.ReadPassword takes a raw file descriptor
// rather than an io.Reader, so there is no way to test the real implementation
// without a TTY. Injecting a Prompter lets `auth`'s field collection be
// table-tested with no terminal at all, which is where the behaviour that
// matters actually lives.
package prompt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Prompter collects input for one command run.
//
// Blank input means "keep the current value" for Line and Secret. That is the
// re-run contract: `openbox auth` re-prompts every field, and pressing Enter
// through all of them must be a no-op rather than a way to erase a credential.
type Prompter interface {
	// Line reads a visible value. current is shown as the default; blank input
	// returns current unchanged.
	Line(prompt, current string) (string, error)
	// Secret reads a value without echoing it. hasCurrent controls whether the
	// prompt offers to keep an existing value; blank input returns "" and the
	// caller keeps what it had.
	Secret(prompt string, hasCurrent bool) (string, error)
	// Confirm asks a yes/no question. defaultYes must be false for anything
	// irreversible.
	Confirm(prompt string, defaultYes bool) (bool, error)
	// Printf writes progress to the prompter's own writer.
	//
	// It is NOT os.Stdout for its own sake: a hook writing to stdout injects
	// context into the coding agent's conversation (INV-3). `auth` is not a
	// hook, but sharing one writer discipline means a helper can never be
	// reused into a hook path and start doing that silently.
	Printf(format string, a ...any)
}

// ErrNotATerminal is returned when input is required but stdin is not a
// terminal and no non-interactive source was named.
//
// This is a deliberate refusal rather than a blocking read. A command that
// blocks on stdin inside CI hangs until the job's global timeout, with no
// output explaining why — the worst failure mode available. Failing in
// milliseconds with the flags to use instead is the whole point.
var ErrNotATerminal = errors.New("stdin is not a terminal")

// NonInteractiveHelp is the remediation text attached to ErrNotATerminal.
//
// It lives here as one constant so the message cannot drift from the flags
// `auth` actually accepts; auth.go's flag definitions and this string are meant
// to be changed together.
const NonInteractiveHelp = `openbox auth needs a terminal to prompt for values.
For automation, name a SOURCE for each secret instead of a value (no secret ever
goes on argv — INV-1):

  printf '%s\n%s\n' "$OBX_KEY" "$OBX_PRIVATE_KEY" |
    openbox auth --api-key-stdin --private-key-stdin --yes

Or set the environment variables directly and skip auth entirely:
  OPENBOX_API_KEY, OPENBOX_AGENT_PRIVATE_KEY, OPENBOX_AGENT_DID, OPENBOX_AGENT_ID`

// New returns a Prompter over a real terminal (or a pipe).
//
// Masking is decided per call from term.IsTerminal rather than once at
// construction, so a caller cannot cache a stale answer about the terminal.
func New(stdin *os.File, out io.Writer) Prompter {
	return &realPrompter{in: stdin, out: out, r: bufio.NewReader(stdin)}
}

type realPrompter struct {
	in  *os.File
	out io.Writer
	r   *bufio.Reader
}

func (p *realPrompter) Printf(format string, a ...any) { fmt.Fprintf(p.out, format, a...) }

// isTerminal reports whether stdin is an interactive terminal.
//
// term.IsTerminal, NOT os.Stdin.Stat(): on Windows a console handle sets
// ModeCharDevice but not ModeDevice (golang/go#23123), so the stdlib mode check
// silently misjudges a real console there. x/term asks the OS directly.
func (p *realPrompter) isTerminal() bool {
	return p.in != nil && term.IsTerminal(int(p.in.Fd()))
}

func (p *realPrompter) Line(promptText, current string) (string, error) {
	if current != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", promptText, current)
	} else {
		fmt.Fprintf(p.out, "%s: ", promptText)
	}
	v, err := p.readLine()
	if err != nil {
		return "", err
	}
	if v == "" {
		return current, nil
	}
	return v, nil
}

func (p *realPrompter) Secret(promptText string, hasCurrent bool) (string, error) {
	if hasCurrent {
		fmt.Fprintf(p.out, "%s [keep current]: ", promptText)
	} else {
		fmt.Fprintf(p.out, "%s: ", promptText)
	}
	if !p.isTerminal() {
		// A piped secret is read plainly — there is no echo to suppress, and
		// refusing here would break the documented --*-stdin automation path.
		v, err := p.readLine()
		if err != nil {
			return "", err
		}
		return v, nil
	}
	raw, err := term.ReadPassword(int(p.in.Fd()))
	// ReadPassword consumes the Enter keypress without echoing it, so the
	// cursor is left mid-line; without this the next prompt appends to it.
	fmt.Fprintln(p.out)
	if err != nil {
		// Never wrap: the error could carry partially-read input.
		return "", fmt.Errorf("read %s: input failed", promptText)
	}
	return strings.TrimRight(string(raw), "\r\n"), nil
}

func (p *realPrompter) Confirm(promptText string, defaultYes bool) (bool, error) {
	hint := "[y/N]"
	if defaultYes {
		hint = "[Y/n]"
	}
	fmt.Fprintf(p.out, "%s %s: ", promptText, hint)
	v, err := p.readLine()
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	case "":
		return defaultYes, nil
	default:
		// Anything unrecognized is NOT taken as consent. A typo must not
		// register an agent or rotate a credential.
		return false, nil
	}
}

// readLine reads one line, trimming the trailing newline and any carriage
// return.
//
// CRLF matters beyond tidiness: a \r left on a base64 signing key fails
// signature verification later with an error naming neither the file nor the
// character. Windows terminals and piped Windows-authored input both produce
// it.
func (p *realPrompter) readLine() (string, error) {
	line, err := p.r.ReadString('\n')
	if err != nil {
		// io.EOF with content is a final line without a trailing newline —
		// normal for piped input, so it is not an error.
		if errors.Is(err, io.EOF) && line != "" {
			return strings.TrimRight(line, "\r\n"), nil
		}
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("unexpected end of input: %w", ErrNotATerminal)
		}
		return "", fmt.Errorf("read input: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// RequireTerminal reports the fail-fast error when interactive input is needed
// and unavailable.
//
// Callers check this BEFORE prompting anything, so a non-interactive run fails
// with the full remediation text rather than after a few fields have already
// been read from whatever the pipe happened to contain.
func RequireTerminal(stdin *os.File) error {
	if stdin != nil && term.IsTerminal(int(stdin.Fd())) {
		return nil
	}
	return fmt.Errorf("%s\n\n%w", NonInteractiveHelp, ErrNotATerminal)
}
