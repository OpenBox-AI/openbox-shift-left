package main

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"

	"github.com/openbox-ai/openbox-shift-left/client/memhttptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHookBinary_ObserveOnlyContract builds and runs the real binary against a
// PreToolUse payload and asserts the observe-only safety contract end-to-end:
// exit 0, EMPTY stdout (so nothing is injected / no block), a spool file is
// written, and — with the content gate closed — no tool_input content reaches the
// spool (INV-2; SL3-SEC-3's unconditional form is retired by ADR-0019 P1).
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
		// Pinned, not inherited. Content capture defaults ON, and since
		// ADR-0019 P1 the observe path carries the tool's input under that
		// gate — so a test that left the posture to the default would be
		// asserting the OPPOSITE of what its name says. Capture OFF is the
		// posture this case is about: the gate closed, nothing carried.
		// The capture-ON side is conformance C36, which asserts on the
		// outbound bytes rather than on the spool.
		"OPENBOX_CONTENT_CAPTURE=0",
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

	// A spool file was written for the session (the sibling "durations/" subdir —
	// the E7-S8 start-time stash — is skipped: we want the session's .jsonl).
	entries, err := os.ReadDir(spoolDir)
	if err != nil {
		t.Fatalf("read spool dir: %v", err)
	}
	var spoolFile string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			if spoolFile != "" {
				t.Fatalf("expected one spool file, got a second: %s", e.Name())
			}
			spoolFile = e.Name()
		}
	}
	if spoolFile == "" {
		t.Fatalf("no session spool file written, entries=%v", entries)
	}
	raw, _ := os.ReadFile(filepath.Join(spoolDir, spoolFile))
	if !strings.Contains(string(raw), "ToolCall") {
		t.Errorf("spooled event should be a ToolCall: %s", raw)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("command content leaked into the spool with content capture OFF: %s", raw)
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

// TestHookBinary_BlockVerdictRecordsAdvisoryExitsZero is the load-bearing
// STORY-SL-9 acceptance test (INV-3): a /evaluate response that returns a BLOCK
// verdict + a guardrail hit produces an advisory record (would_block=true, the
// guardrail category) AND the hook STILL exits 0 with EMPTY stdout — nothing is
// denied, delayed, or errored. The record carries no tool content (INV-2).
func TestHookBinary_BlockVerdictRecordsAdvisoryExitsZero(t *testing.T) {
	memhttptest.RequireBind(t)
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "openbox-cc-hook")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	// A core that would BLOCK, with a guardrail hit (categories only).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"verdict":"block","risk_score":0.91,"trust_tier":2,` +
			`"guardrails_result":{"validation_passed":false,` +
			`"reasons":[{"type":"pii","field":"email","reason":"Contains PII"}]}}`))
	}))
	defer srv.Close()

	advPath := filepath.Join(dir, "advisories.jsonl")
	seed := base64.StdEncoding.EncodeToString(make([]byte, 32)) // valid 32-byte Ed25519 seed
	secret := "SECRET-COMMAND-do-not-egress"
	baseEnv := append(os.Environ(),
		"OPENBOX_AGENT_DID=did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		"OPENBOX_BASE_URL="+srv.URL, // loopback http is allowed (INV-1 check)
		"OPENBOX_API_KEY=obx_test_key",
		"OPENBOX_ED25519_SEED="+seed,
		"OPENBOX_SPOOL_DIR="+filepath.Join(dir, "spool"),
		"OPENBOX_ADVISORY_FILE="+advPath,
		"OPENBOX_SESSION_DIR="+filepath.Join(dir, "sessions"),
		"OPENBOX_CONFIG="+filepath.Join(dir, "none.json"),
		// Observe mode, stated rather than inherited. This case is about a BLOCK
		// verdict staying ADVISORY, which is only true with enforce off — and
		// enforce defaults ON (ADR-0016). It passed on the default before only
		// because the gate also needed the tier-2 toggle, which defaulted off;
		// ADR-0017 removed that toggle, so the mode has to be explicit.
		"OPENBOX_ENFORCE=0",
	)

	run := func(hook, payload string) {
		cmd := exec.Command(bin, hook)
		cmd.Stdin = strings.NewReader(payload)
		cmd.Env = baseEnv
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s must exit 0 despite a BLOCK verdict (INV-3), got %v\nstderr: %s", hook, err, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("%s stdout must be EMPTY (no block / no injection), got %q", hook, stdout.String())
		}
	}

	// Spool a tool call (content in tool_input), then SessionEnd flushes it.
	run("PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"sess-xyz","cwd":"/r","tool_name":"Bash","tool_input":{"command":"`+secret+`"}}`)
	run("SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"sess-xyz","cwd":"/r","reason":"logout"}`)

	raw, err := os.ReadFile(advPath)
	if err != nil {
		t.Fatalf("advisory sink not written on BLOCK: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"would_block":true`) {
		t.Errorf("advisory missing would_block=true:\n%s", body)
	}
	if !strings.Contains(body, `"type":"pii"`) {
		t.Errorf("advisory missing the guardrail category:\n%s", body)
	}
	// INV-1/INV-2: no tool content, no secret in the record.
	if strings.Contains(body, secret) {
		t.Fatalf("content leaked into advisory sink: %s", body)
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
