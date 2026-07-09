package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHookBinary_ObserveOnlyContract builds and runs the real binary against a
// PreToolUse payload and asserts the observe-only safety contract end-to-end:
// exit 0, EMPTY stdout (so nothing is injected / no block), a spool file is
// written, and content from tool_input never reaches the spool (INV-2/SL3-SEC-3).
func TestHookBinary_ObserveOnlyContract(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "openbox-cc-hook")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}

	spoolDir := filepath.Join(dir, "spool")
	secret := "TOP-SECRET-COMMAND-do-not-egress"
	payload := `{"hook_event_name":"PreToolUse","session_id":"sess-xyz","cwd":"/r","tool_name":"Bash","tool_input":{"command":"` + secret + `"}}`

	cmd := exec.Command(bin, "PreToolUse")
	cmd.Stdin = strings.NewReader(payload)
	cmd.Env = append(os.Environ(),
		"OPENBOX_AGENT_DID=did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		"OPENBOX_SPOOL_DIR="+spoolDir,
		"OPENBOX_CONFIG="+filepath.Join(dir, "none.json"),
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("binary must exit 0 (observe-only), got %v\nstderr: %s", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout must be empty (no context injection / no block), got %q", stdout.String())
	}

	// A spool file was written for the session.
	entries, err := os.ReadDir(spoolDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one spool file, got %v (err %v)", entries, err)
	}
	raw, _ := os.ReadFile(filepath.Join(spoolDir, entries[0].Name()))
	if !strings.Contains(string(raw), "ToolCall") {
		t.Errorf("spooled event should be a ToolCall: %s", raw)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("command content leaked into the spool: %s", raw)
	}
}

// TestHookBinary_MaintainsSessionRegistry proves the SL-5 wiring: a hook touches
// this session's liveness record (so the git prepare-commit-msg hook can attribute
// a commit to it), carrying only structural fields (session_id + cwd, no content),
// and SessionEnd removes it.
func TestHookBinary_MaintainsSessionRegistry(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "openbox-cc-hook")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	regDir := filepath.Join(dir, "sessions")
	baseEnv := append(os.Environ(),
		"OPENBOX_AGENT_DID=did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		"OPENBOX_SPOOL_DIR="+filepath.Join(dir, "spool"),
		"OPENBOX_CONFIG="+filepath.Join(dir, "none.json"),
		"OPENBOX_SESSION_DIR="+regDir,
	)
	run := func(hook, payload string) {
		cmd := exec.Command(bin, hook)
		cmd.Stdin = strings.NewReader(payload)
		cmd.Env = baseEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s must exit 0, got %v\n%s", hook, err, out)
		}
	}

	secret := "SECRET-COMMAND"
	run("PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"sess-xyz","cwd":"/work/repo","tool_name":"Bash","tool_input":{"command":"`+secret+`"}}`)

	entries, err := os.ReadDir(regDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one session record, got %v (err %v)", entries, err)
	}
	rec, _ := os.ReadFile(filepath.Join(regDir, entries[0].Name()))
	if !strings.Contains(string(rec), "sess-xyz") || !strings.Contains(string(rec), "/work/repo") {
		t.Fatalf("record missing structural fields: %s", rec)
	}
	if strings.Contains(string(rec), secret) {
		t.Fatalf("content leaked into session record: %s", rec)
	}

	// SessionEnd removes the record.
	run("SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"sess-xyz","cwd":"/work/repo","reason":"other"}`)
	if entries, _ := os.ReadDir(regDir); len(entries) != 0 {
		t.Fatalf("SessionEnd should remove the record, still have %v", entries)
	}
}

// TestHookBinary_AmbientGitHookInstall proves the opt-in ambient install: on
// SessionStart with OPENBOX_INSTALL_GIT_HOOK set, the adapter installs the
// prepare-commit-msg hook into the session's repo (pointing at the sibling
// openbox-git-hook); without the flag it does nothing (default-safe).
func TestHookBinary_AmbientGitHookInstall(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "openbox-cc-hook")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	repo := filepath.Join(dir, "repo")
	os.MkdirAll(repo, 0o755)
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	hookPath := filepath.Join(repo, ".git", "hooks", "prepare-commit-msg")
	payload := `{"hook_event_name":"SessionStart","session_id":"sess-1","cwd":"` + repo + `","source":"startup"}`
	baseEnv := append(os.Environ(),
		"OPENBOX_AGENT_DID=did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		"OPENBOX_SPOOL_DIR="+filepath.Join(dir, "spool"),
		"OPENBOX_CONFIG="+filepath.Join(dir, "none.json"),
		"OPENBOX_SESSION_DIR="+filepath.Join(dir, "sessions"),
	)

	// Default (flag unset): no hook installed.
	c := exec.Command(bin, "SessionStart")
	c.Stdin = strings.NewReader(payload)
	c.Env = baseEnv
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("exit 0 expected: %v\n%s", err, out)
	}
	if _, err := os.Stat(hookPath); err == nil {
		t.Fatal("hook installed without OPENBOX_INSTALL_GIT_HOOK (should be default-off)")
	}

	// Opt-in: hook installed, pointing at the sibling engine binary.
	c = exec.Command(bin, "SessionStart")
	c.Stdin = strings.NewReader(payload)
	c.Env = append(baseEnv, "OPENBOX_INSTALL_GIT_HOOK=1")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("exit 0 expected: %v\n%s", err, out)
	}
	body, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("hook not installed with opt-in: %v", err)
	}
	// STORY-SL4-WIRE-2: the ambient install now points the prepare-commit-msg
	// hook back at the unified engine as `<engine> hook git prepare-commit-msg`
	// (no separate openbox-git-hook binary), tagged by the managed marker.
	//
	// NOTE: this asserts the baked ARGS only — the binary under test is the legacy
	// cc-hook alias, which does not itself parse `hook git …` (it would no-op,
	// fail-open). The FUNCTIONAL install→commit→stamp path is proven on the real
	// unified binary by cli TestUnifiedBinaryGitHookStampsCommit.
	if !strings.Contains(string(body), "'hook' 'git' 'prepare-commit-msg'") || !strings.Contains(string(body), "openbox-shift-left") {
		t.Fatalf("installed hook does not re-invoke the unified engine:\n%s", body)
	}
}

// TestHookBinary_NoArgsIsSafe confirms a misinvocation still exits 0.
func TestHookBinary_NoArgsIsSafe(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "openbox-cc-hook")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "OPENBOX_CONFIG="+filepath.Join(dir, "none.json"))
	if err := cmd.Run(); err != nil {
		t.Fatalf("no-args invocation must still exit 0, got %v", err)
	}
}
