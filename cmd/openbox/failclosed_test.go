package main

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	muse "github.com/openbox-ai/openbox-shift-left/internal/adapters/muse"
)

// readOnlyMachine is a HOME and an OpenBox home that cannot be written to, and
// a record of everything under them, so a test can prove a code path touched
// neither. The successor runs exactly when the gate failed, which may be
// because the home is unwritable.
func readOnlyMachine(t *testing.T) (home, openboxHome string) {
	t.Helper()
	isolateHome(t)
	root := t.TempDir()
	home = filepath.Join(root, "home")
	openboxHome = filepath.Join(root, "ro", "openbox-home") // does not exist
	for _, d := range []string{home, filepath.Dir(openboxHome)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv(devconfig.EnvHome, openboxHome)
	t.Setenv(devconfig.EnvConfigPath, "")
	for _, d := range []string{home, filepath.Dir(openboxHome)} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
		dir := d
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	}
	return home, openboxHome
}

func filesCreated(t *testing.T, roots ...string) []string {
	t.Helper()
	var found []string
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && path != root {
				found = append(found, path)
			}
			return nil
		})
	}
	return found
}

func TestMuseFailClosedDeniesGatedEventsAndTouchesNothing(t *testing.T) {
	home, openboxHome := readOnlyMachine(t)
	for _, argv := range [][]string{
		{"muse", "--fail-closed", "UserPromptSubmit"},
		{"muse", "--fail-closed", "PreToolUse"},
		{"muse", "--fail-closed", "PermissionRequest"},
		{"muse", "--fail-closed", "PreLLMCall"},
		{"muse", "--home", openboxHome, "--fail-closed", "PreToolUse"},
		{"muse", "--home", "relative/home", "--fail-closed", "PreLLMCall"},
	} {
		a, out, errb := testApp(nil)
		if code := a.runHook(argv); code != muse.FaultExitCode {
			t.Errorf("%v: exit = %d, want %d", argv, code, muse.FaultExitCode)
		}
		if out.Len() != 0 {
			t.Errorf("%v: stdout = %q; Muse would have to reject it", argv, out.String())
		}
		if lines := strings.Count(errb.String(), "\n"); lines != 1 {
			t.Errorf("%v: stderr is %d lines, want one reason: %q", argv, lines, errb.String())
		}
	}
	if created := filesCreated(t, home, filepath.Dir(openboxHome)); len(created) != 0 {
		t.Errorf("the successor wrote under the homes: %v", created)
	}
	if _, err := os.Stat(openboxHome); err == nil {
		t.Error("the OpenBox home was created")
	}
}

// With no config, no identity and no network, exactly what a broken install
// looks like, the successor still answers: it needs none of them.
func TestMuseFailClosedNeedsNoConfigIdentityOrNetwork(t *testing.T) {
	isolateHome(t) // nothing seeded: no credentials, no dev.json
	t.Setenv(devconfig.EnvBaseURL, "http://127.0.0.1:1")
	a, _, _ := testApp(nil)
	if code := a.runHook([]string{"muse", "--fail-closed", "PreToolUse"}); code != muse.FaultExitCode {
		t.Fatalf("exit = %d", code)
	}
}

func TestMuseFailClosedPassesEventsThatGateNothing(t *testing.T) {
	readOnlyMachine(t)
	for _, ev := range []string{"SessionStart", "PostToolUse", "PostLLMCall", "SessionEnd", "Stop"} {
		a, out, errb := testApp(nil)
		if code := a.runHook([]string{"muse", "--fail-closed", ev}); code != exitOK || out.Len() != 0 || errb.Len() != 0 {
			t.Errorf("%s: exit %d stdout %q stderr %q; it gates nothing", ev, code, out.String(), errb.String())
		}
	}
}

func TestMuseFailClosedWithoutAnEventRefuses(t *testing.T) {
	readOnlyMachine(t)
	for _, argv := range [][]string{{"muse", "--fail-closed"}, {"muse", "--fail-closed", "Bogus"}} {
		a, _, errb := testApp(nil)
		if code := a.runHook(argv); code != muse.FaultExitCode || errb.Len() == 0 {
			t.Errorf("%v: exit %d stderr %q", argv, code, errb.String())
		}
	}
}

// The route is offered by the engine, not keyed on a provider name: a provider
// whose engine has no fail-closed form sees its arguments unchanged.
func TestFailClosedFlagIsNotSpecialForOtherProviders(t *testing.T) {
	isolateHome(t)
	var seen []string
	withHookEngine(t, fakeEngine{run: func(event string) { seen = append(seen, event) }})
	for _, name := range []string{"claude-code", "codex"} {
		a, _, _ := testApp(nil)
		if code := a.runHook([]string{name, "--fail-closed", "PreToolUse"}); code != exitOK {
			t.Errorf("%s: exit = %d", name, code)
		}
	}
	if len(seen) != 2 || seen[0] != "--fail-closed" || seen[1] != "--fail-closed" {
		t.Errorf("the engine was handed %v; the flag must reach it as before", seen)
	}
}

// A panic in the successor must still deny.
func TestMuseFailClosedPanicStillDenies(t *testing.T) {
	isolateHome(t)
	withHookEngine(t, panickingFailClosed{})
	a, _, errb := testApp(nil)
	if code := a.runHook([]string{"muse", "--fail-closed", "PreToolUse"}); code == exitOK {
		t.Fatalf("a crashed successor exited 0; stderr %q", errb.String())
	}
}

type panickingFailClosed struct{ fakeEngine }

func (panickingFailClosed) RunFailClosed(string, io.Writer) int {
	panic("boom")
}

// Read-only homes prove little on their own: a write that fails leaves nothing
// either way. Here the homes are writable, so a stray trace line, store or
// lock would show up as a file.
func TestMuseFailClosedCreatesNothingWhereItCould(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	openboxHome := filepath.Join(root, "openbox-home")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv(devconfig.EnvHome, openboxHome)
	t.Setenv(devconfig.EnvConfigPath, "")
	if err := os.MkdirAll(filepath.Join(root, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := filesCreated(t, root)
	a, _, _ := testApp(nil)
	if code := a.runHook([]string{"muse", "--home", openboxHome, "--fail-closed", "PreToolUse"}); code != muse.FaultExitCode {
		t.Fatalf("exit = %d", code)
	}
	if after := filesCreated(t, root); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("the successor changed the tree: before %v, after %v", before, after)
	}
}

// TestHookArgvIsDocumented holds the documented argv table to the parser. The
// two flags are read by runHook, not declared on a flag set, so the CI step
// that cross-checks documented flags against declared ones cannot see them.
func TestHookArgvIsDocumented(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, want := range []string{
		"openbox hook <provider> [--home <abs dir>] <event>",
		"openbox hook <provider> [--home <abs dir>] --fail-closed <event>",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("cmd/openbox/README.md does not document %q", want)
		}
	}
	// And the usage line runHook prints agrees with the documented shape.
	a, _, errb := testApp(nil)
	isolateHome(t)
	a.runHook([]string{"muse", "--home", t.TempDir()})
	if !strings.Contains(errb.String(), "openbox hook muse [--home <abs dir>] <event>") {
		t.Errorf("the usage line drifted from the documented argv: %q", errb.String())
	}
}
