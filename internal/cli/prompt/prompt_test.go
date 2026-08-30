package prompt

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pipeStdin writes body to a temp file and returns it opened for reading, which
// is a non-terminal *os.File — the shape a piped or redirected stdin has.
func pipeStdin(t *testing.T, body string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestLine(t *testing.T) {
	for _, tc := range []struct {
		name, input, current, want string
	}{
		{name: "typed value wins", input: "acme\n", current: "local", want: "acme"},
		{name: "blank keeps current", input: "\n", current: "local", want: "local"},
		{name: "blank with no current is empty", input: "\n", current: "", want: ""},
		{name: "CRLF is trimmed", input: "acme\r\n", current: "local", want: "acme"},
		{name: "final line without a newline still reads", input: "acme", current: "local", want: "acme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			p := New(pipeStdin(t, tc.input), &out)
			got, err := p.Line("Organization", tc.current)
			if err != nil {
				t.Fatalf("Line: %v", err)
			}
			if got != tc.want {
				t.Errorf("Line = %q, want %q", got, tc.want)
			}
		})
	}
}

// The current value is shown so a re-run tells the user what pressing Enter will
// keep. A prompt that hides it makes blank-keeps-current unusable.
func TestLineShowsCurrentValue(t *testing.T) {
	var out bytes.Buffer
	p := New(pipeStdin(t, "\n"), &out)
	if _, err := p.Line("Organization", "acme"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "[acme]") {
		t.Errorf("prompt should show the current value, got %q", out.String())
	}
}

// A non-terminal stdin reads plainly: there is no echo to suppress, and refusing
// here would break the documented --*-stdin automation path.
func TestSecretReadsPipedInputAndNeverEchoesIt(t *testing.T) {
	var out bytes.Buffer
	p := New(pipeStdin(t, "obx_SENTINEL_SECRET\n"), &out)
	got, err := p.Secret("API key", false)
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	if got != "obx_SENTINEL_SECRET" {
		t.Errorf("Secret = %q", got)
	}
	if strings.Contains(out.String(), "SENTINEL") {
		t.Errorf("the secret was echoed to the writer: %q", out.String())
	}
}

func TestSecretTrimsCRLF(t *testing.T) {
	var out bytes.Buffer
	p := New(pipeStdin(t, "YmFzZTY0\r\n"), &out)
	got, err := p.Secret("Private key", false)
	if err != nil {
		t.Fatal(err)
	}
	// A \r left on a base64 signing key fails signature verification later with
	// an error naming neither the file nor the character.
	if got != "YmFzZTY0" {
		t.Errorf("Secret = %q, want no trailing carriage return", got)
	}
}

func TestSecretOffersToKeepAnExistingValue(t *testing.T) {
	var out bytes.Buffer
	p := New(pipeStdin(t, "\n"), &out)
	if _, err := p.Secret("API key", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "keep current") {
		t.Errorf("prompt should offer to keep the current secret, got %q", out.String())
	}
}

func TestConfirm(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		defaultYes  bool
		want        bool
	}{
		{name: "y", input: "y\n", want: true},
		{name: "yes", input: "yes\n", want: true},
		{name: "Y uppercase", input: "Y\n", want: true},
		{name: "n", input: "n\n", defaultYes: true, want: false},
		{name: "blank takes the default (no)", input: "\n", defaultYes: false, want: false},
		{name: "blank takes the default (yes)", input: "\n", defaultYes: true, want: true},
		// A typo must never register an agent or rotate a credential.
		{name: "garbage is not consent", input: "maybe\n", defaultYes: true, want: false},
		{name: "whitespace around yes", input: "  y  \n", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			p := New(pipeStdin(t, tc.input), &out)
			got, err := p.Confirm("Register a new agent?", tc.defaultYes)
			if err != nil {
				t.Fatalf("Confirm: %v", err)
			}
			if got != tc.want {
				t.Errorf("Confirm = %v, want %v", got, tc.want)
			}
		})
	}
}

// Anything irreversible must default to No, so the hint has to show it.
func TestConfirmHintShowsTheDefault(t *testing.T) {
	for _, tc := range []struct {
		defaultYes bool
		want       string
	}{{false, "[y/N]"}, {true, "[Y/n]"}} {
		var out bytes.Buffer
		p := New(pipeStdin(t, "\n"), &out)
		if _, err := p.Confirm("Rotate credentials?", tc.defaultYes); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("defaultYes=%v hint = %q, want %q", tc.defaultYes, out.String(), tc.want)
		}
	}
}

// Fail fast, never hang. A command that blocks on stdin inside CI hangs until the
// job's global timeout with no output explaining why.
func TestRequireTerminalFailsFastOnAPipe(t *testing.T) {
	err := RequireTerminal(pipeStdin(t, ""))
	if err == nil {
		t.Fatal("RequireTerminal accepted a non-terminal stdin")
	}
	if !errors.Is(err, ErrNotATerminal) {
		t.Errorf("error should wrap ErrNotATerminal, got %v", err)
	}
	// The remediation must name the automation path, or the error is a dead end.
	for _, want := range []string{"--api-key-stdin", "--private-key-stdin", "OPENBOX_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("remediation missing %q:\n%s", want, err)
		}
	}
}

func TestRequireTerminalRejectsNilStdin(t *testing.T) {
	if err := RequireTerminal(nil); err == nil {
		t.Fatal("RequireTerminal(nil) should not report a usable terminal")
	}
}

// Running out of input is an explicit error, not an empty string that would be
// read as "the user pressed Enter" and silently keep a stale credential.
func TestExhaustedInputErrors(t *testing.T) {
	var out bytes.Buffer
	p := New(pipeStdin(t, ""), &out)
	if _, err := p.Line("Organization", "local"); err == nil {
		t.Fatal("expected an error when stdin is empty")
	}
}

// --- the scripted prompter, which auth's own tests depend on ----------------

func TestScriptedAnswersInOrderAndRecordsPrompts(t *testing.T) {
	s := &Scripted{Answers: []string{"acme", "", "obx_k", "y"}}

	if got, _ := s.Line("Organization", "local"); got != "acme" {
		t.Errorf("Line = %q", got)
	}
	if got, _ := s.Line("Backend URL", "https://api.openbox.ai"); got != "https://api.openbox.ai" {
		t.Errorf("blank should keep the current value, got %q", got)
	}
	if got, _ := s.Secret("API key", false); got != "obx_k" {
		t.Errorf("Secret = %q", got)
	}
	if ok, _ := s.Confirm("Proceed?", false); !ok {
		t.Error("Confirm should be true for y")
	}
	if s.Remaining() != 0 {
		t.Errorf("Remaining = %d, want 0", s.Remaining())
	}
	want := []string{"Organization", "Backend URL", "API key", "Proceed?"}
	if len(s.Prompts) != len(want) {
		t.Fatalf("Prompts = %v, want %v", s.Prompts, want)
	}
	for i := range want {
		if s.Prompts[i] != want[i] {
			t.Errorf("Prompts[%d] = %q, want %q", i, s.Prompts[i], want[i])
		}
	}
}

// A test whose prompt count drifted from the implementation must fail loudly
// rather than quietly exercise "the user pressed Enter" for the rest.
func TestScriptedExhaustionIsAnError(t *testing.T) {
	s := &Scripted{Answers: []string{"one"}}
	if _, err := s.Line("First", ""); err != nil {
		t.Fatal(err)
	}
	_, err := s.Line("Second", "")
	if err == nil {
		t.Fatal("expected an error once the script ran out")
	}
	if !strings.Contains(err.Error(), "Second") {
		t.Errorf("error should name the unanswered prompt, got %v", err)
	}
}

// Remaining is how a test proves a short-circuit happened (a blank agent id
// skipping the credential prompts) rather than assuming it did.
func TestScriptedRemainingDetectsAShortCircuit(t *testing.T) {
	s := &Scripted{Answers: []string{"a", "b", "c"}}
	if _, err := s.Line("only one asked", ""); err != nil {
		t.Fatal(err)
	}
	if s.Remaining() != 2 {
		t.Errorf("Remaining = %d, want 2", s.Remaining())
	}
}

// No implementation may write a secret to its own writer.
func TestNoSecretIsEverWrittenToTheWriter(t *testing.T) {
	const sentinel = "obx_NEVER_PRINT_ME"
	s := &Scripted{Answers: []string{sentinel}}
	if _, err := s.Secret("API key", false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.Out.String(), sentinel) {
		t.Errorf("scripted prompter echoed a secret: %q", s.Out.String())
	}
}
