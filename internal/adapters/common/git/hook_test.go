package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallHook_WritesExecutableScript(t *testing.T) {
	dir := t.TempDir()
	if err := InstallHook(dir, HookConfig{Command: "/opt/openbox/bin/openbox-git-hook"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "prepare-commit-msg")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("hook not executable: %v", info.Mode())
	}
	body, _ := os.ReadFile(path)
	s := string(body)
	for _, want := range []string{"#!/bin/sh", managedMarker, "exit 0", "/opt/openbox/bin/openbox-git-hook", "'prepare-commit-msg'"} {
		if !strings.Contains(s, want) {
			t.Fatalf("hook script missing %q:\n%s", want, s)
		}
	}
}

func TestInstallHook_IdempotentOnOwnHook(t *testing.T) {
	dir := t.TempDir()
	if err := InstallHook(dir, HookConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := InstallHook(dir, HookConfig{}); err != nil {
		t.Fatalf("re-install of our own hook should succeed: %v", err)
	}
}

func TestInstallHook_RefusesForeignHook(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prepare-commit-msg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho someone elses hook\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := InstallHook(dir, HookConfig{}); err == nil {
		t.Fatal("expected refusal to overwrite a foreign hook")
	}
	// The foreign hook must be left intact.
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "someone elses hook") {
		t.Fatalf("foreign hook was clobbered:\n%s", body)
	}
}

func TestHookScript_CustomCommandAndArgs(t *testing.T) {
	s := hookScript(HookConfig{Command: "openbox", Args: []string{"hook", "git", "prepare-commit-msg"}})
	// The OD17 unified-engine invocation must render safely.
	if !strings.Contains(s, "'hook' 'git' 'prepare-commit-msg'") {
		t.Fatalf("unified-engine args not rendered:\n%s", s)
	}
	if !strings.Contains(s, "OPENBOX_GIT_HOOK='openbox'") {
		t.Fatalf("command default not rendered:\n%s", s)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Fatalf("shellQuote = %s", got)
	}
}

// RunPrepareCommitMsg is fail-open at the arg layer: no message-file arg is a
// logged no-op, never an error that would surface to git.
func TestRunPrepareCommitMsg_NoArgsIsNoop(t *testing.T) {
	g := Git{}
	n, err := g.RunPrepareCommitMsg(nil, SessionResolver{}, nil)
	if err != nil || n != 0 {
		t.Fatalf("got n=%d err=%v, want 0,nil", n, err)
	}
}

// TestHookConfigDefaultsToTheShippedBinary holds a default that outlived what it
// named. It once produced `openbox-git-hook`, a dev instrument that shipped in no
// release and that no installer targeted, so a zero-valued HookConfig wrote a
// hook invoking a binary present on no machine. Production always sets Command,
// which is exactly why nothing noticed.
func TestHookConfigDefaultsToTheShippedBinary(t *testing.T) {
	var c HookConfig
	if got := c.command(); got != "openbox" {
		t.Errorf("default command = %q, want the shipped binary %q", got, "openbox")
	}
	want := []string{"hook", "git", "prepare-commit-msg"}
	got := c.args()
	if len(got) != len(want) {
		t.Fatalf("default args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("default args = %v, want %v", got, want)
		}
	}
}
