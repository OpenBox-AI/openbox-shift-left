package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var bin string

// ambientSessionEnv is the agent context that must not reach these tests: the
// hook resolves a session from it (Tier-0/Tier-1 in git.SessionResolver), and
// every case here spawns the binary with os.Environ(), so a suite run from
// inside a live Codex session would stamp that session instead of the fixture's
// (report SL-11). Unsetting before m.Run keeps it out of the child env too.
var ambientSessionEnv = []string{
	"CODEX_THREAD_ID",
	"OPENBOX_SESSION",
	"OPENBOX_SESSION_FILE",
	"OPENBOX_SESSION_TTL",
}

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		os.Exit(0) // interpret-trailers unavailable; skip
	}
	for _, k := range ambientSessionEnv {
		os.Unsetenv(k)
	}
	dir, err := os.MkdirTemp("", "obgh")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "openbox-git-hook")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic("build: " + err.Error())
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func run(t *testing.T, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return string(out), code
}

// The binary MUST always exit 0 — a non-zero prepare-commit-msg aborts the
// developer's commit (the git analog of SL-4's INV-3 contract).
func TestBinary_AlwaysExitsZero(t *testing.T) {
	cases := [][]string{
		{},                                      // no args
		{"bogus-subcommand"},                    // unknown
		{"prepare-commit-msg"},                  // missing message-file arg
		{"prepare-commit-msg", "/no/such/file"}, // unreadable message file
	}
	for _, args := range cases {
		if _, code := run(t, sessEnv("sess-A"), args...); code != 0 {
			t.Fatalf("args %v exited %d, want 0", args, code)
		}
	}
}

func TestBinary_StampsMessageFile(t *testing.T) {
	msg := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	if err := os.WriteFile(msg, []byte("subject\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, code := run(t, sessEnv("sess-A"), "prepare-commit-msg", msg); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	data, _ := os.ReadFile(msg)
	if !strings.Contains(string(data), "OpenBox-Session: sess-A") {
		t.Fatalf("message not stamped:\n%s", data)
	}
}

// A secret-shaped value must never be written, and the binary still exits 0.
func TestBinary_NeverStampsSecret(t *testing.T) {
	msg := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	os.WriteFile(msg, []byte("subject\n"), 0o644)
	if _, code := run(t, sessEnv("obx_secret"), "prepare-commit-msg", msg); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	data, _ := os.ReadFile(msg)
	if strings.Contains(string(data), "obx_") {
		t.Fatalf("secret leaked:\n%s", data)
	}
}

func sessEnv(id string) []string { return []string{"OPENBOX_SESSION=" + id} }

// TestBinary_InstallStampsCommit proves the legacy alias's `install` bakes the
// OLD `<self> prepare-commit-msg "$@"` form and that a real commit is stamped
// through it — the alias counterpart to the unified `openbox hook git install`
// end-to-end test (STORY-SL4-WIRE-2 F2). Guards against a regression to the
// alias's baked installArgs.
func TestBinary_InstallStampsCommit(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitEnv := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	git := func(env []string, args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(gitEnv, "init", "-q")
	git(gitEnv, "config", "user.email", "t@example.com")
	git(gitEnv, "config", "user.name", "t")

	// Install via the alias binary (cwd = repo so it finds the hooks dir).
	inst := exec.Command(bin, "install")
	inst.Dir = repo
	inst.Env = gitEnv
	if out, err := inst.CombinedOutput(); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(repo, ".git", "hooks", "prepare-commit-msg"))
	if err != nil {
		t.Fatalf("hook not installed: %v", err)
	}
	// Alias bakes the OLD form: `<self> prepare-commit-msg "$@"` (not `hook git …`).
	if !strings.Contains(string(body), "'prepare-commit-msg'") || strings.Contains(string(body), "'hook' 'git'") {
		t.Fatalf("alias install baked the wrong form:\n%s", body)
	}

	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(gitEnv, "add", ".")
	commit := exec.Command("git", "-C", repo, "commit", "-q", "-m", "subject")
	commit.Env = append(gitEnv, "OPENBOX_SESSION=sess-alias")
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	msg := exec.Command("git", "-C", repo, "log", "-1", "--format=%B")
	msg.Env = gitEnv
	out, err := msg.CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OpenBox-Session: sess-alias") {
		t.Fatalf("commit not stamped via alias install:\n%s", out)
	}
}
