package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// STORY-SL7-A AC-8: CODEX_THREAD_ID — injected by Codex into every exec env —
// is the highest-precedence session source, needs no registry, and leaves the
// existing (Claude Code) resolution untouched when absent.

func TestResolve_CodexThreadIDWinsOutright(t *testing.T) {
	env := map[string]string{
		EnvCodexThreadID: "0195c7e4-2222-7000-8000-00000000abcd",
		EnvSession:       "explicit-override",
	}
	r := SessionResolver{Getenv: func(k string) string { return env[k] }}
	got := r.Resolve("/any/worktree")
	if len(got) != 1 || got[0] != "0195c7e4-2222-7000-8000-00000000abcd" {
		t.Fatalf("Resolve = %v, want the CODEX_THREAD_ID (highest precedence, over OPENBOX_SESSION)", got)
	}
}

func TestResolve_CodexThreadIDNeedsNoWorktreeOrRegistry(t *testing.T) {
	env := map[string]string{EnvCodexThreadID: "th-42"}
	r := SessionResolver{Getenv: func(k string) string { return env[k] }}
	// Even with no resolvable worktree (registry tier unusable) it attributes.
	if got := r.Resolve(""); len(got) != 1 || got[0] != "th-42" {
		t.Fatalf("Resolve(\"\") = %v, want [th-42]", got)
	}
}

func TestResolve_AbsentCodexThreadIDLeavesExistingTiersUntouched(t *testing.T) {
	// Tier 1 still wins when only OPENBOX_SESSION is set.
	env := map[string]string{EnvSession: "cc-session-1"}
	r := SessionResolver{Getenv: func(k string) string { return env[k] }}
	if got := r.Resolve("/wt"); len(got) != 1 || got[0] != "cc-session-1" {
		t.Fatalf("Resolve = %v, want the OPENBOX_SESSION tier unchanged", got)
	}

	// Tier 2 (registry recency) still resolves with no env at all.
	dir := t.TempDir()
	now := time.Now()
	if err := WriteSessionRecord(dir, "cc-live-session", "/wt/sub", now); err != nil {
		t.Fatal(err)
	}
	r2 := SessionResolver{
		Getenv:     func(string) string { return "" },
		SessionDir: dir,
		Now:        func() time.Time { return now },
	}
	if got := r2.Resolve("/wt"); len(got) != 1 || got[0] != "cc-live-session" {
		t.Fatalf("registry tier broken: %v", got)
	}
}

// End-to-end: with CODEX_THREAD_ID in the env, StampMessageFile stamps exactly
// that id as the OpenBox-Session trailer (the sink still validates the id).
func TestPrepareCommitMsg_StampsFromCodexThreadID(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	g := Git{Dir: dir}
	if _, err := g.run("init", "-q"); err != nil {
		t.Skipf("git init: %v", err)
	}
	msg := filepath.Join(dir, "COMMIT_EDITMSG")
	if err := os.WriteFile(msg, []byte("feat: add endpoint\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{EnvCodexThreadID: "0195c7e4-3333-7000-8000-00000000cafe"}
	r := SessionResolver{Getenv: func(k string) string { return env[k] }}
	if _, err := g.RunPrepareCommitMsg([]string{msg}, r, t.Logf); err != nil {
		t.Fatalf("prepare-commit-msg: %v", err)
	}
	got, err := g.ReadTrailers(msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "0195c7e4-3333-7000-8000-00000000cafe" {
		t.Fatalf("trailers = %v, want the codex thread id stamped", got)
	}

	// A secret-shaped value is still refused by the sink (INV-1).
	msg2 := filepath.Join(dir, "COMMIT_EDITMSG2")
	_ = os.WriteFile(msg2, []byte("feat: two\n"), 0o600)
	env[EnvCodexThreadID] = "obx_live_notasession"
	if _, err := g.RunPrepareCommitMsg([]string{msg2}, r, t.Logf); err != nil {
		t.Fatalf("prepare-commit-msg: %v", err)
	}
	if got, _ := g.ReadTrailers(msg2); len(got) != 0 {
		t.Fatalf("secret-shaped id must never be stamped, got %v", got)
	}
}
