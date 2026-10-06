// Package prompt is the interactive input layer for `openbox auth` and
// `openbox init`'s adopt question: plain
// lines, masked secrets, and yes/no confirmation.
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

// Prompter collects input for one command run. Blank input means "keep the
// current value" for Line and Secret. That is the re-run contract: `openbox
// auth` re-prompts every field, and pressing Enter through all of them must be
// a no-op rather than a way to erase a credential.
type Prompter interface {
	// Line reads a visible value. Current is shown as the default; blank input
	// returns current unchanged.
	Line(prompt, current string) (string, error)
	// Secret reads a value without echoing it. HasCurrent controls whether the
	// prompt offers to keep an existing value; blank input returns "" and the
	// caller keeps what it had.
	Secret(prompt string, hasCurrent bool) (string, error)
	// Confirm asks a yes/no question. DefaultYes must be false for anything
	// irreversible.
	Confirm(prompt string, defaultYes bool) (bool, error)
	// Printf writes progress to the prompter's own writer. `auth` is not a hook,
	// but sharing one writer discipline means a helper can never be reused into a
	// hook path and start doing that silently.
	Printf(format string, a ...any)
}

// ErrNotATerminal is returned when input is required but stdin is not a
// terminal and no non-interactive source was named.
var ErrNotATerminal = errors.New("stdin is not a terminal")

// NonInteractiveHelp is the remediation text attached to ErrNotATerminal.
//
// It lives here as one constant so the message cannot drift from what `auth`
// actually accepts. `auth` now accepts nothing: it prompts, or it refuses. Both
// routes below outrank anything it would have written anyway -- a real
// environment variable beats both files at read time, and the files are the
// files -- so neither is a lesser path.
const NonInteractiveHelp = `openbox auth needs a terminal: it prompts, and it takes no flags.
Three routes provision a machine without one, and all are read in preference to
anything auth writes (no secret ever goes on argv; INV-1):

  1. Export the org token and let 'init' register each tool's agent. This is
     the one to reach for; it is the only route that mints an identity:
       OPENBOX_CONTROL_TOKEN=…  openbox init --provider <tool>

  2. Export an agent identity directly. Note the limit: these variables outrank
     every store at once, so one exported DID makes EVERY governed tool report
     the same identity. Fine for a single-tool CI image, wrong for a developer
     machine running two:
       OPENBOX_API_KEY, OPENBOX_AGENT_PRIVATE_KEY, OPENBOX_AGENT_DID, OPENBOX_AGENT_ID

  3. Write the files by hand; one store per governed tool. This is also the
     key-rotation route:
       ~/.openbox/<tool>/.env      0600, secrets only:
                                     OPENBOX_API_KEY, OPENBOX_AGENT_PRIVATE_KEY
       ~/.openbox/<tool>/dev.json  coordinates only:
                                     agent_id, developer_did, base_url, backend_url
       ~/.openbox/.env             0600, the org control token only
       ~/.openbox/dev.json         the organization's base_url and backend_url
     OPENBOX_HOME relocates all of it. Keep secrets and coordinates separate: a
     secret in dev.json or a coordinate in .env reintroduces a stale-copy bug
     that reverted a corrected DID on every install.`

// New returns a Prompter over a real terminal (or a pipe). Masking is decided
// per call from term.IsTerminal rather than once at construction, so a caller
// cannot cache a stale answer about the terminal.
func New(stdin *os.File, out io.Writer) Prompter {
	return &realPrompter{in: stdin, out: out, r: bufio.NewReader(stdin)}
}

type realPrompter struct {
	in  *os.File
	out io.Writer
	r   *bufio.Reader
}

func (p *realPrompter) Printf(format string, a ...any) { fmt.Fprintf(p.out, format, a...) }

// isTerminal term.IsTerminal, NOT os.Stdin.Stat(): on Windows a console handle
// sets ModeCharDevice but not ModeDevice (golang/go#23123), so the stdlib mode
// check silently misjudges a real console there. X/term asks the OS directly.
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
		v, err := p.readLine()
		if err != nil {
			return "", err
		}
		return v, nil
	}
	raw, err := term.ReadPassword(int(p.in.Fd()))
	fmt.Fprintln(p.out)
	if err != nil {
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
	return confirmed(v, defaultYes), nil
}

func (p *realPrompter) readLine() (string, error) {
	line, err := p.r.ReadString('\n')
	if err != nil {
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
func RequireTerminal(stdin *os.File) error {
	if stdin != nil && term.IsTerminal(int(stdin.Fd())) {
		return nil
	}
	return fmt.Errorf("%s\n\n%w", NonInteractiveHelp, ErrNotATerminal)
}

// confirmed maps a typed answer onto the yes/no contract: blank takes the
// default, and anything unrecognized is "no" -- a typo must not register an
// agent or rotate a credential. Both prompters share it, so the scripted one
// cannot drift from what a developer actually sees.
func confirmed(answer string, defaultYes bool) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	case "":
		return defaultYes
	default:
		return false
	}
}
