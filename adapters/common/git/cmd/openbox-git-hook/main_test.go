package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var bin string

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		os.Exit(0) // interpret-trailers unavailable; skip
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
