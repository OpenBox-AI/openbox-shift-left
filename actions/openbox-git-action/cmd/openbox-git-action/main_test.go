package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// initRepo makes a throwaway repo with one trailer-bearing commit and returns
// its dir + HEAD sha.
func initRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.name", "Dev")
	run("config", "user.email", "dev@example.com")
	run("config", "commit.gpgsign", "false")
	msg := filepath.Join(dir, "m")
	if err := os.WriteFile(msg, []byte("ship\n\nOpenBox-Session: sess-A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("commit", "--allow-empty", "--cleanup=verbatim", "-F", msg)
	sha := revParse(t, dir)
	return dir, sha
}

func revParse(t *testing.T, dir string) string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestCLI_DryRunResolvesWithoutCreds(t *testing.T) {
	dir, sha := initRepo(t)
	var out, errb bytes.Buffer
	// No OPENBOX_* creds set: --dry-run must NOT require them.
	code := run([]string{"--dry-run", "--dir", dir, "--sha", sha, "--repo", "o/r", "--environment", "staging"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", code, errb.String())
	}
	var payload struct {
		Resolution map[string]any `json:"resolution"`
		Event      map[string]any `json:"event"`
		Emitted    bool           `json:"emitted"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("dry-run output is not JSON: %v\n%s", err, out.String())
	}
	if payload.Emitted {
		t.Fatal("dry-run must not emit")
	}
	if payload.Resolution["commit_sha"] != sha {
		t.Fatalf("resolved commit_sha = %v, want %s", payload.Resolution["commit_sha"], sha)
	}
}

func TestCLI_MissingSHAisUsageError(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"--dry-run"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage)", code)
	}
}

func TestCLI_MissingCredsIsPreconditionError(t *testing.T) {
	dir, sha := initRepo(t)
	// Not a dry-run and no creds ⇒ client.New fails ⇒ exit 2 (operator fixes it),
	// never a silent success or a CI break disguised as a telemetry drop.
	for _, k := range []string{"OPENBOX_BASE_URL", "OPENBOX_API_KEY", "OPENBOX_DID", "OPENBOX_SEED"} {
		t.Setenv(k, "")
	}
	var out, errb bytes.Buffer
	code := run([]string{"--dir", dir, "--sha", sha}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (missing creds precondition)\nstderr:\n%s", code, errb.String())
	}
}
