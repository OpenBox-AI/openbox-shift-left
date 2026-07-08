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
