package prompt

import (
	"bytes"
	"fmt"
	"strings"
)

// scripted.go — the test implementation, in the non-test build on purpose.
//
// `openbox auth` lives in package main, so it cannot import a _test.go helper
// from here. Exporting Scripted is what lets auth's field collection be
// table-tested through the same interface production uses, which is the reason
// the interface exists at all (term.ReadPassword takes a raw fd, so the real
// implementation cannot be driven without a TTY).

// Scripted answers prompts from a fixed list, in order, and records what it was
// shown. Exhausting the list is an error rather than an empty string: a test
// whose prompt count drifted from the implementation's should fail loudly, not
// silently exercise "the user pressed Enter".
type Scripted struct {
	Answers []string
	// Out captures everything written, so a test can assert on the prompt text
	// AND on the absence of a secret in it.
	Out bytes.Buffer

	// Prompts records each prompt string in the order shown, so a test can pin
	// the field order — which is a UX contract in `auth`, not an accident.
	Prompts []string

	i int
}

// next pops the next scripted answer.
func (s *Scripted) next(promptText string) (string, error) {
	s.Prompts = append(s.Prompts, promptText)
	if s.i >= len(s.Answers) {
		return "", fmt.Errorf("scripted prompter exhausted at %q (%d answers supplied) — "+
			"the code asked for more input than the test scripted", promptText, len(s.Answers))
	}
	v := s.Answers[s.i]
	s.i++
	// Trim exactly as the real prompter does, so a test cannot pass on input the
	// real one would have cleaned differently.
	return strings.TrimRight(v, "\r\n"), nil
}

func (s *Scripted) Line(promptText, current string) (string, error) {
	fmt.Fprintf(&s.Out, "%s [%s]: ", promptText, current)
	v, err := s.next(promptText)
	if err != nil {
		return "", err
	}
	if v == "" {
		return current, nil
	}
	return v, nil
}

func (s *Scripted) Secret(promptText string, hasCurrent bool) (string, error) {
	fmt.Fprintf(&s.Out, "%s [secret]: ", promptText)
	v, err := s.next(promptText)
	if err != nil {
		return "", err
	}
	_ = hasCurrent
	return v, nil
}

func (s *Scripted) Confirm(promptText string, defaultYes bool) (bool, error) {
	fmt.Fprintf(&s.Out, "%s [confirm]: ", promptText)
	v, err := s.next(promptText)
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
		return false, nil
	}
}

func (s *Scripted) Printf(format string, a ...any) { fmt.Fprintf(&s.Out, format, a...) }

// Remaining reports how many scripted answers were not consumed. A test that
// expected a short-circuit (a blank agent id skipping the credential prompts,
// say) asserts on this rather than trusting that it happened.
func (s *Scripted) Remaining() int { return len(s.Answers) - s.i }
