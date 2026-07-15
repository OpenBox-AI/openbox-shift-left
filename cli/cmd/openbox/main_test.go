package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	claudecode "github.com/openbox-ai/openbox-shift-left/adapters/claude-code"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/backend"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/devinit"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/secret"
	"github.com/openbox-ai/openbox-shift-left/sidecar"
)

// fakeReg implements devinit.Registrar for the command-wiring tests.
type fakeReg struct {
	reg    *backend.Registration
	create int
}

func (f *fakeReg) Create(context.Context, backend.CreateAgentRequest) (*backend.Registration, error) {
	f.create++
	return f.reg, nil
}
func (f *fakeReg) FindByName(context.Context, string) (*backend.AgentSummary, error) {
	return nil, nil
}

// testApp builds an app with in-memory writers and seams that fail loudly if a
// path touches the environment/keychain/network when it should not.
func testApp(env map[string]string) (*app, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	a := &app{
		stdout: &out,
		stderr: &errb,
		getenv: func(k string) string { return env[k] },
		openStore: func(string) (secret.Store, error) {
			return nil, errors.New("openStore should not be called in this path")
		},
		newRegistrar: func(_, _, _ string) devinit.Registrar {
			panic("newRegistrar should not be called in this path")
		},
	}
	return a, &out, &errb
}

func TestVersion(t *testing.T) {
	a, out, _ := testApp(nil)
	if code := a.run([]string{"version"}); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "openbox ") {
		t.Errorf("version output = %q", out.String())
	}
}

func TestDryRunIsOfflineAndNeedsNoToken(t *testing.T) {
	a, out, _ := testApp(nil) // empty env; detect/registrar seams panic if touched
	code := a.run([]string{"dev", "init", "--provider", "claude-code", "--dry-run", "--org", "acme"})
	if code != exitOK {
		t.Fatalf("dry-run exit = %d", code)
	}
	if !strings.Contains(out.String(), "DRY RUN") {
		t.Errorf("missing DRY RUN banner: %q", out.String())
	}
}

func TestMissingTokenIsINV1Guard(t *testing.T) {
	a, _, errb := testApp(nil)
	code := a.run([]string{"dev", "init", "--provider", "claude-code", "--backend-url", "https://x"})
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(errb.String(), "OPENBOX_CONTROL_TOKEN") || !strings.Contains(errb.String(), "INV-1") {
		t.Errorf("expected INV-1 token guard, got %q", errb.String())
	}
}

func TestNoSecretStoreHalts(t *testing.T) {
	a, _, errb := testApp(map[string]string{"OPENBOX_CONTROL_TOKEN": "obx_key_x", "OPENBOX_BACKEND_URL": "https://x"})
	a.openStore = func(string) (secret.Store, error) { return nil, secret.ErrNoStore }
	code := a.run([]string{"dev", "init", "--provider", "claude-code"})
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(errb.String(), "HALT") {
		t.Errorf("expected HALT on no secret store, got %q", errb.String())
	}
	// The HALT must name BOTH escape hatches (install a keyring, or --secret-backend file).
	if !strings.Contains(errb.String(), "--secret-backend file") {
		t.Errorf("HALT should point to the file backend escape hatch, got %q", errb.String())
	}
}

func TestFileBackendSelectedByFlag(t *testing.T) {
	// Opting into the file backend must NOT HALT — it resolves to a usable store.
	// (Registration then fails on the fake network, but past the secret-store gate.)
	a, _, errb := testApp(map[string]string{"OPENBOX_CONTROL_TOKEN": "obx_key_x", "OPENBOX_BACKEND_URL": "https://x"})
	var gotKind string
	store := secret.NewMemStore()
	a.openStore = func(kind string) (secret.Store, error) { gotKind = kind; return store, nil }
	a.newRegistrar = func(_, _, _ string) devinit.Registrar {
		return &fakeReg{reg: &backend.Registration{AgentID: "a", DID: "did:aip:x", APIKey: "obx_test_k", PrivateKey: "seed"}}
	}
	// codex is still a stub (SL-7 unbuilt), so this exercises the file-backend
	// gate without materializing the real claude-code bundle into $HOME.
	code := a.run([]string{"dev", "init", "--provider", "codex", "--org", "acme", "--secret-backend", "file"})
	if gotKind != "file" {
		t.Fatalf("openStore kind = %q, want file", gotKind)
	}
	if code == exitError && strings.Contains(errb.String(), "HALT") {
		t.Fatalf("file backend must not HALT: %q", errb.String())
	}
	if !strings.Contains(errb.String(), "PLAINTEXT") {
		t.Errorf("expected a plaintext warning for the file backend, got %q", errb.String())
	}
}

func TestConfigManualOnlyExitsTwo(t *testing.T) {
	a, _, errb := testApp(map[string]string{"OPENBOX_CONTROL_TOKEN": "obx_key_x", "OPENBOX_BACKEND_URL": "https://x"})
	store := secret.NewMemStore()
	a.openStore = func(string) (secret.Store, error) { return store, nil }
	a.newRegistrar = func(_, _, _ string) devinit.Registrar {
		return &fakeReg{reg: &backend.Registration{AgentID: "a", DID: "did:aip:x", APIKey: "obx_test_k", PrivateKey: "seed"}}
	}
	// codex adapter is not built -> registered but config-manual -> exit 2.
	code := a.run([]string{"dev", "init", "--provider", "codex", "--org", "acme"})
	if code != exitConfigOnly {
		t.Fatalf("exit = %d, want %d", code, exitConfigOnly)
	}
	if !strings.Contains(errb.String(), "note:") {
		t.Errorf("expected a note on partial success, got %q", errb.String())
	}
	// Credentials must have landed in the store.
	if store.Len() != 3 {
		t.Errorf("stored %d secrets, want 3", store.Len())
	}
}

// TestClaudeCodeInstallsForRealExitsZero proves the SL4-WIRE-1 front door: the
// CLI registers the real claudecode.Installer (not the SL-2 stub), so
// `dev init --provider claude-code` materializes the plugin bundle + dev config
// and exits 0. HOME and OPENBOX_CONFIG are redirected to temp dirs (the
// installer reads them directly) so nothing lands in the developer's real home.
func TestClaudeCodeInstallsForRealExitsZero(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "openbox", "dev.json")
	t.Setenv("HOME", home)
	t.Setenv("OPENBOX_CONFIG", cfgPath)

	a, out, errb := testApp(map[string]string{"OPENBOX_CONTROL_TOKEN": "obx_key_x", "OPENBOX_BACKEND_URL": "https://x"})
	store := secret.NewMemStore()
	a.openStore = func(string) (secret.Store, error) { return store, nil }
	a.newRegistrar = func(_, _, _ string) devinit.Registrar {
		return &fakeReg{reg: &backend.Registration{AgentID: "a", DID: "did:aip:x", APIKey: "obx_test_k", PrivateKey: "c2VlZA=="}}
	}

	code := a.run([]string{"dev", "init", "--provider", "claude-code", "--org", "acme"})
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitOK, errb.String())
	}
	if !strings.Contains(out.String(), "Wrote claude-code native config") {
		t.Errorf("expected a config-applied message, got %q", out.String())
	}

	// Bundle + dev config materialized under the redirected HOME / config path.
	if _, err := os.Stat(filepath.Join(home, ".claude", "plugins", "openbox-observe", ".claude-plugin", "plugin.json")); err != nil {
		t.Errorf("plugin bundle not materialized: %v", err)
	}
	// STORY-SL4-WIRE-2 AC3, proven through the real `dev init` front door: the
	// running engine is placed at bin/openbox (providers.Lookup → os.Executable()
	// → Installer.EngineBinary), executable, so the hooks' ${…}/bin/openbox resolves.
	enginePath := filepath.Join(home, ".claude", "plugins", "openbox-observe", "bin", "openbox")
	if fi, err := os.Stat(enginePath); err != nil {
		t.Errorf("engine not placed at bin/openbox via dev init: %v", err)
	} else if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("placed engine is not executable: %v", fi.Mode())
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read dev config: %v", err)
	}
	// INV-1: the written config carries coordinates, never the secret values.
	if strings.Contains(string(raw), "obx_test_k") || strings.Contains(string(raw), "c2VlZA==") {
		t.Errorf("dev config leaked a secret value:\n%s", raw)
	}
	if store.Len() != 3 {
		t.Errorf("stored %d secrets, want 3", store.Len())
	}
}

// A claude-code --dry-run must render the real installer's plan but write
// NOTHING to disk (no bundle, no config), even though claude-code is now a real
// installer. HOME/OPENBOX_CONFIG are redirected so any accidental write would
// land — and be caught — under temp dirs.
func TestClaudeCodeDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "openbox", "dev.json")
	t.Setenv("HOME", home)
	t.Setenv("OPENBOX_CONFIG", cfgPath)

	a, out, _ := testApp(nil) // no token/store/registrar: dry-run must stay offline
	code := a.run([]string{"dev", "init", "--provider", "claude-code", "--dry-run", "--org", "acme"})
	if code != exitOK {
		t.Fatalf("dry-run exit = %d", code)
	}
	// The plan came from the REAL installer (observe-only plugin), not the stub.
	if !strings.Contains(out.String(), "OpenBox Claude Code plugin (observe-only") {
		t.Errorf("dry-run did not render the real installer plan:\n%s", out.String())
	}
	// Nothing was written.
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Errorf("dry-run created a plugin dir under HOME (err=%v)", err)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("dry-run wrote the dev config (err=%v)", err)
	}
}

// --- STORY-SL4-WIRE-2: unified `openbox hook claude-code <event>` -----------

// setHookEnv points the (os.Getenv-based) hook engine at temp dirs so no real
// spool/registry/config is touched. Returns the spool dir.
func setHookEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	t.Setenv("OPENBOX_AGENT_DID", "did:aip:7f3c9b2e-0000-5000-a000-000000000001")
	t.Setenv("OPENBOX_SPOOL_DIR", spool)
	t.Setenv("OPENBOX_SESSION_DIR", filepath.Join(dir, "sessions"))
	t.Setenv("OPENBOX_CONFIG", filepath.Join(dir, "none.json"))
	return spool
}

// TestHookIsObserveOnlyInProcess drives the unified subcommand in-process and
// asserts the INV-3 contract: exit 0, EMPTY stdout, event spooled, no content
// (tool_input) leaked into the spool.
func TestHookIsObserveOnlyInProcess(t *testing.T) {
	spool := setHookEnv(t)
	a, out, errb := testApp(nil)
	secret := "TOP-SECRET-do-not-egress"
	a.stdin = strings.NewReader(`{"hook_event_name":"PreToolUse","session_id":"s1","cwd":"/r","tool_name":"Bash","tool_input":{"command":"` + secret + `"}}`)

	code := a.run([]string{"hook", "claude-code", "PreToolUse"})
	if code != exitOK {
		t.Fatalf("hook exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout must be empty (no context injection / no block), got %q", out.String())
	}
	raw, _ := os.ReadFile(filepath.Join(spool, onlySpoolFile(t, spool)))
	if strings.Contains(string(raw), secret) {
		t.Fatalf("command content leaked into the spool: %s", raw)
	}
}

// TestHookMisuseIsSafe: a bad/missing provider or event still exits 0 with empty
// stdout (never block, never inject). Diagnostics go to stderr.
func TestHookMisuseIsSafe(t *testing.T) {
	setHookEnv(t)
	for _, args := range [][]string{
		{"hook"},
		{"hook", "claude-code"},
		{"hook", "vim", "PreToolUse"},
	} {
		a, out, errb := testApp(nil)
		a.stdin = strings.NewReader("")
		if code := a.run(args); code != exitOK {
			t.Errorf("%v exit = %d, want 0", args, code)
		}
		if out.Len() != 0 {
			t.Errorf("%v wrote to stdout: %q", args, out.String())
		}
		_ = errb
	}
}

// TestUnifiedBinaryHookObserveOnlyContract is the G_SEC re-verify: the SL-4
// exit-0/empty-stdout contract must survive folding the hook into the
// multi-command `openbox` binary. It builds and runs the REAL binary.
func TestUnifiedBinaryHookObserveOnlyContract(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "openbox")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build openbox: %v\n%s", err, out)
	}
	spool := filepath.Join(dir, "spool")
	secret := "TOP-SECRET-do-not-egress"
	payload := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/r","tool_name":"Bash","tool_input":{"command":"` + secret + `"}}`

	cmd := exec.Command(bin, "hook", "claude-code", "PreToolUse")
	cmd.Stdin = strings.NewReader(payload)
	cmd.Env = append(os.Environ(),
		"OPENBOX_AGENT_DID=did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		"OPENBOX_SPOOL_DIR="+spool,
		"OPENBOX_CONFIG="+filepath.Join(dir, "none.json"),
		"OPENBOX_SESSION_DIR="+filepath.Join(dir, "sessions"),
	)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("`openbox hook` must exit 0 (observe-only), got %v\nstderr: %s", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout must be empty on the unified binary, got %q", stdout.String())
	}
	spoolFile := onlySpoolFile(t, spool)
	if raw, _ := os.ReadFile(filepath.Join(spool, spoolFile)); strings.Contains(string(raw), secret) {
		t.Fatalf("content leaked into the spool: %s", raw)
	}
}

// onlySpoolFile returns the single session .jsonl spool file under dir, ignoring
// the companion "durations/" subdir (the E7-S8 start-time stash).
func onlySpoolFile(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read spool dir: %v", err)
	}
	var found string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if found != "" {
			t.Fatalf("expected one spool file, got a second: %s", e.Name())
		}
		found = e.Name()
	}
	if found == "" {
		t.Fatalf("no session spool file written, entries=%v", entries)
	}
	return found
}

// TestHookEndToEndSmoke drives all five hooks through the unified subcommand and
// asserts the whole observe→deliver path on the unified binary (in-process):
//   - the HOT-PATH hooks (SessionStart..PostToolUse) never block on the network
//     — they spool locally and cause ZERO egress; delivery happens only at
//     SessionEnd (AC4 latency budget: the async/no-network-on-hot-path guarantee);
//   - each hot-path hook returns well within a coarse wall-clock budget;
//   - SessionEnd flushes the spooled session to /evaluate and drains the spool;
//   - CONTENT never reaches the wire: a canary planted in tool_input.command is
//     absent from every body delivered to /evaluate (INV-2 on the egress bytes,
//     not just the spool).
func TestHookEndToEndSmoke(t *testing.T) {
	const contentCanary = "SECRET-EGRESS-CANARY-do-not-send"
	var mu sync.Mutex
	got := 0
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/governance/evaluate" {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			got++
			bodies = append(bodies, string(b))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"governance_event_id":"ge","verdict":"allow","risk_score":0.1,"action":"continue","fallback_used":false}`))
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	t.Setenv("OPENBOX_AGENT_DID", "did:aip:7f3c9b2e-0000-5000-a000-000000000001")
	t.Setenv("OPENBOX_SPOOL_DIR", spool)
	t.Setenv("OPENBOX_SESSION_DIR", filepath.Join(dir, "sessions"))
	t.Setenv("OPENBOX_CONFIG", filepath.Join(dir, "none.json"))
	t.Setenv("OPENBOX_BASE_URL", srv.URL)
	t.Setenv("OPENBOX_API_KEY", "obx_test_"+strings.Repeat("a", 48))
	t.Setenv("OPENBOX_ED25519_SEED", "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")

	events := []struct {
		hook, payload string
		hotPath       bool // hot-path hooks must NOT egress and must be fast
	}{
		{"SessionStart", `{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/r","source":"startup"}`, true},
		{"UserPromptSubmit", `{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"/r","prompt":"hi"}`, true},
		{"PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"s1","cwd":"/r","tool_name":"Bash","tool_input":{"command":"` + contentCanary + `"}}`, true},
		{"PostToolUse", `{"hook_event_name":"PostToolUse","session_id":"s1","cwd":"/r","tool_name":"Bash","tool_response":{"ok":true}}`, true},
		{"SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"s1","cwd":"/r","reason":"other"}`, false},
	}
	// A generous hot-path budget: local spool only, so this catches a regression
	// that introduces synchronous/network work on the hot path without CI flake.
	const hotPathBudget = 2 * time.Second
	for _, e := range events {
		a, out, errb := testApp(nil)
		a.stdin = strings.NewReader(e.payload)
		start := time.Now()
		code := a.run([]string{"hook", "claude-code", e.hook})
		elapsed := time.Since(start)
		if code != exitOK {
			t.Fatalf("%s exit = %d; stderr=%q", e.hook, code, errb.String())
		}
		if out.Len() != 0 {
			t.Fatalf("%s wrote to stdout: %q", e.hook, out.String())
		}
		if e.hotPath {
			if elapsed > hotPathBudget {
				t.Errorf("%s hot-path hook took %v (> budget %v) — is it blocking on the network?", e.hook, elapsed, hotPathBudget)
			}
			mu.Lock()
			n := got
			mu.Unlock()
			if n != 0 {
				t.Fatalf("hot-path hook %s caused egress (%d /evaluate calls) — the hot path must be async/no-network (NFR-2)", e.hook, n)
			}
		}
	}

	mu.Lock()
	n := got
	delivered := append([]string(nil), bodies...)
	mu.Unlock()
	if n == 0 {
		t.Fatalf("mock /evaluate received no events — SessionEnd flush did not deliver through the unified binary")
	}
	// The session's spool FILES must be drained after a successful flush (the
	// "durations/" subdir — the E7-S8 stash, swept per-session at SessionEnd — is
	// not a spool file).
	drained, _ := os.ReadDir(spool)
	for _, e := range drained {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			t.Errorf("spool not drained after SessionEnd flush: %s", e.Name())
		}
	}
	// INV-2 on the wire: no delivered body may carry the tool_input content.
	for i, b := range delivered {
		if strings.Contains(b, contentCanary) {
			t.Fatalf("content canary leaked to /evaluate in delivered body #%d:\n%s", i, b)
		}
	}
	// Activity label survives the spool round-trip to the wire: the Bash tool call
	// lands activity_type="Bash" (not "Unknown"), and a lifecycle event lands its
	// event_type. Derived client-side from persisted fields, so spooling keeps it.
	all := strings.Join(delivered, "\n")
	if !strings.Contains(all, `"activity_type":"Bash"`) {
		t.Errorf("no delivered body carried activity_type=Bash (tool label lost across spool):\n%s", all)
	}
	if !strings.Contains(all, `"activity_type":"SessionStarted"`) {
		t.Errorf("no delivered body carried activity_type=SessionStarted (lifecycle label):\n%s", all)
	}
}

// TestUnifiedBinaryGitHookStampsCommit proves the OD17 git-hook fold end-to-end:
// `openbox hook git install` writes a prepare-commit-msg hook that re-invokes the
// unified binary as `openbox hook git prepare-commit-msg`, and a real commit gets
// the OpenBox-Session trailer stamped — with no separate openbox-git-hook binary.
func TestUnifiedBinaryGitHookStampsCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary + runs git; skipped in -short")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "openbox")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build openbox: %v\n%s", err, out)
	}
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitEnv := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	git := func(env []string, args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = env
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git(gitEnv, "init", "-q")
	git(gitEnv, "config", "user.email", "t@example.com")
	git(gitEnv, "config", "user.name", "t")

	// Install the hook via the UNIFIED binary (openbox hook git install).
	ic := exec.Command(bin, "hook", "git", "install")
	ic.Dir = repo
	if out, err := ic.CombinedOutput(); err != nil {
		t.Fatalf("openbox hook git install: %v\n%s", err, out)
	}
	hookBody, err := os.ReadFile(filepath.Join(repo, ".git", "hooks", "prepare-commit-msg"))
	if err != nil {
		t.Fatalf("hook not installed: %v", err)
	}
	if !strings.Contains(string(hookBody), "'hook' 'git' 'prepare-commit-msg'") {
		t.Fatalf("installed hook does not re-invoke `openbox hook git`:\n%s", hookBody)
	}

	// A real commit with a session in scope must be stamped by the unified engine.
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(gitEnv, "add", ".")
	commit := exec.Command("git", "-C", repo, "commit", "-q", "-m", "subject")
	commit.Env = append(gitEnv, "OPENBOX_SESSION=sess-unified")
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	if body := git(gitEnv, "log", "-1", "--format=%B"); !strings.Contains(body, "OpenBox-Session: sess-unified") {
		t.Fatalf("commit not stamped by `openbox hook git prepare-commit-msg`:\n%s", body)
	}
}

// --- STORY-SL-11: `openbox dev verify` --------------------------------------

// verifyTestSeed is a known base64 raw 32-byte Ed25519 seed (same fixture the
// hook E2E test uses). The verify tests drive the real signer through the CLI and
// check the signature server-side against the public key derived from it.
const verifyTestSeed = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

const verifyTestDID = "did:aip:00000000-0000-0000-0000-000000000042"

// coreValidateOK verifies the AIP-signed GET /api/v1/auth/validate exactly as
// openbox-core would (empty-body SHA, canonical GET string, Ed25519 verify) and
// answers 200; a bad signature → 401. It stands in for the real core.
func coreValidateOK(t *testing.T, seedB64 string) *httptest.Server {
	t.Helper()
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil {
		t.Fatalf("decode seed: %v", err)
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != client.AuthValidatePath {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		wantSHA := hex.EncodeToString(sum[:])
		canonical := "GET\n" + r.URL.Path + "\n" +
			r.Header.Get("X-OpenBox-Agent-Timestamp") + "\n" +
			r.Header.Get("X-OpenBox-Agent-Nonce") + "\n" + wantSHA
		sig, decErr := base64.StdEncoding.DecodeString(r.Header.Get("X-OpenBox-Agent-Signature"))
		if r.Header.Get("X-OpenBox-Body-SHA256") != wantSHA || decErr != nil ||
			!ed25519.Verify(pub, []byte(canonical), sig) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"code":401,"message":"invalid token"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"valid":true,"active":true,"agent_id":"a","environment":"test","message":"ok"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// setVerifyCreds points the adapter resolvers at direct-env credentials + a temp
// (nonexistent) config so no real keychain/config is touched.
func setVerifyCreds(t *testing.T, baseURL string) {
	t.Helper()
	t.Setenv("OPENBOX_CONFIG", filepath.Join(t.TempDir(), "none.json"))
	t.Setenv("OPENBOX_BASE_URL", baseURL)
	t.Setenv("OPENBOX_AGENT_DID", verifyTestDID)
	t.Setenv("OPENBOX_API_KEY", "obx_test_"+strings.Repeat("a", 48))
	t.Setenv("OPENBOX_ED25519_SEED", verifyTestSeed)
}

// TestDevVerifyHappyPath: a valid key + signing round-trip against the mock core
// prints a ✓ line naming the DID + base_url and exits 0.
func TestDevVerifyHappyPath(t *testing.T) {
	srv := coreValidateOK(t, verifyTestSeed)
	setVerifyCreds(t, srv.URL)

	a, out, errb := testApp(nil)
	code := a.run([]string{"dev", "verify"})
	if code != exitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "✓ verified:") ||
		!strings.Contains(out.String(), verifyTestDID) ||
		!strings.Contains(out.String(), srv.URL) {
		t.Errorf("expected a ✓ line naming DID + base_url, got %q", out.String())
	}
	// INV-1: no secret in the success output.
	if strings.Contains(out.String(), verifyTestSeed) || strings.Contains(out.String(), "obx_test_") {
		t.Errorf("INV-1: secret leaked into ✓ output: %q", out.String())
	}
}

// TestDevVerifyBadKeyIsMappedFailure: core rejects the identity (401) → a ✗ with
// the mapped fix hint on stderr and a non-zero exit; no secret leaks.
func TestDevVerifyBadKeyIsMappedFailure(t *testing.T) {
	// A server that always 401s, regardless of the signature (simulates a wrong
	// key / unprovisioned agent).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":401,"message":"invalid token"}`)
	}))
	defer srv.Close()
	setVerifyCreds(t, srv.URL)

	a, out, errb := testApp(nil)
	code := a.run([]string{"dev", "verify"})
	if code == exitOK {
		t.Fatalf("bad key must exit non-zero, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "✗") || !strings.Contains(errb.String(), "identity rejected") {
		t.Errorf("expected a ✗ with a mapped reason, got %q", errb.String())
	}
	if strings.Contains(errb.String(), verifyTestSeed) {
		t.Errorf("INV-1: secret leaked into ✗ output: %q", errb.String())
	}
}

// TestDevVerifyDryRunIsOffline: --dry-run prints the plan (method, path,
// base_url, DID) and makes NO network call — the registrar/store seams panic if
// touched, and no creds are configured.
func TestDevVerifyDryRunIsOffline(t *testing.T) {
	t.Setenv("OPENBOX_CONFIG", filepath.Join(t.TempDir(), "none.json"))
	t.Setenv("OPENBOX_BASE_URL", "https://core.example.test")
	t.Setenv("OPENBOX_AGENT_DID", verifyTestDID)

	a, out, _ := testApp(nil)
	code := a.run([]string{"dev", "verify", "--dry-run"})
	if code != exitOK {
		t.Fatalf("dry-run exit = %d", code)
	}
	got := out.String()
	for _, want := range []string{"DRY RUN", "GET ", client.AuthValidatePath, "https://core.example.test", verifyTestDID} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run plan missing %q; got %q", want, got)
		}
	}
	// --print-plan is an accepted alias.
	a2, out2, _ := testApp(nil)
	if code := a2.run([]string{"dev", "verify", "--print-plan"}); code != exitOK {
		t.Fatalf("--print-plan exit = %d", code)
	}
	if !strings.Contains(out2.String(), "DRY RUN") {
		t.Errorf("--print-plan did not render the plan: %q", out2.String())
	}
}

// TestDevVerifyNoCredsSaysInitFirst: with nothing configured, verify exits
// non-zero and tells the operator to run `openbox dev init` (never half-proceeds).
func TestDevVerifyNoCredsSaysInitFirst(t *testing.T) {
	t.Setenv("OPENBOX_CONFIG", filepath.Join(t.TempDir(), "none.json"))
	// Ensure no ambient creds bleed in from the developer's real environment.
	for _, k := range []string{"OPENBOX_BASE_URL", "OPENBOX_AGENT_DID", "OPENBOX_API_KEY", "OPENBOX_ED25519_SEED", "OPENBOX_SECRET_FILE"} {
		t.Setenv(k, "")
	}
	a, _, errb := testApp(nil)
	code := a.run([]string{"dev", "verify"})
	if code != exitError {
		t.Fatalf("no-creds exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(errb.String(), "openbox dev init") {
		t.Errorf("expected a 'run openbox dev init' hint, got %q", errb.String())
	}
}

func TestUnknownProviderAndMissingProvider(t *testing.T) {
	a, _, _ := testApp(nil)
	if code := a.run([]string{"dev", "init", "--provider", "vim", "--dry-run"}); code != exitError {
		t.Errorf("unknown provider exit = %d", code)
	}
	if code := a.run([]string{"dev", "init", "--dry-run"}); code != exitError {
		t.Errorf("missing provider exit = %d", code)
	}
}

// ── STORY-E6-S8: `openbox dev sync` ──────────────────────────────────────────

type fakePolicyReader struct {
	pol *backend.Policy
	err error
}

func (f *fakePolicyReader) GetCurrentPolicy(context.Context, string) (*backend.Policy, error) {
	return f.pol, f.err
}

// syncApp builds an app whose env + policy reader are controllable, with getenv
// wired to os.Getenv so the adapter resolvers (ResolveBackendURL/AgentID/Bundle)
// and runDevSync's token read see the same t.Setenv values.
func syncApp(reader policyReader) (*app, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	a := &app{
		stdout:          &out,
		stderr:          &errb,
		getenv:          os.Getenv,
		newPolicyReader: func(_, _, _ string) policyReader { return reader },
	}
	return a, &out, &errb
}

func TestDevSync_BuilderPolicyWritesPinnedBundle(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundle.json")
	t.Setenv("OPENBOX_CONFIG", filepath.Join(dir, "none.json"))
	t.Setenv("OPENBOX_CONTROL_TOKEN", "obx_key_secretorgkey")
	t.Setenv("OPENBOX_BACKEND_URL", "https://backend.example")
	t.Setenv("OPENBOX_AGENT_ID", "agent-9")
	t.Setenv("OPENBOX_SIDECAR_BUNDLE", bundlePath)
	t.Setenv("OPENBOX_STALE_DIR", filepath.Join(dir, "stale"))

	reader := &fakePolicyReader{pol: &backend.Policy{
		ID:            "pol-42",
		UpdatedAt:     "2026-07-15T12:00:00Z",
		PolicyBuilder: []byte(`{"version":1,"rules":[{"decision":"BLOCK","reason":"no rm","matchMode":"all","conditions":[{"field":"spans[_].attributes.command","operator":"contains","transform":"value","value":"rm -rf","valueType":"string"}]}]}`),
	}}
	a, out, errb := syncApp(reader)

	if code := a.run([]string{"dev", "sync"}); code != exitOK {
		t.Fatalf("dev sync exit = %d, want 0 (stderr=%s)", code, errb.String())
	}
	raw, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("bundle not written: %v", err)
	}
	// The written bundle carries the PIN + the builder config, and is loadable.
	b, err := sidecar.ParseBundle(raw)
	if err != nil {
		t.Fatalf("written bundle invalid: %v", err)
	}
	if b.PolicyID != "pol-42" || b.UpdatedAt != "2026-07-15T12:00:00Z" || b.PolicyBuilder == nil {
		t.Errorf("bundle pin/config wrong: %+v", b)
	}
	// INV-1: the org key must never appear in stdout/stderr.
	if strings.Contains(out.String(), "obx_key_") || strings.Contains(errb.String(), "obx_key_") {
		t.Errorf("org key leaked to output")
	}
	// 0600 owner-only.
	if fi, _ := os.Stat(bundlePath); fi != nil && fi.Mode().Perm() != 0o600 {
		t.Errorf("bundle perm = %v, want 0600", fi.Mode().Perm())
	}
}

func TestDevSync_NullPolicyWritesAllowBundle(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundle.json")
	t.Setenv("OPENBOX_CONFIG", filepath.Join(dir, "none.json"))
	t.Setenv("OPENBOX_CONTROL_TOKEN", "obx_key_x")
	t.Setenv("OPENBOX_BACKEND_URL", "https://b.example")
	t.Setenv("OPENBOX_AGENT_ID", "a")
	t.Setenv("OPENBOX_SIDECAR_BUNDLE", bundlePath)

	a, _, errb := syncApp(&fakePolicyReader{pol: nil}) // data==null
	if code := a.run([]string{"dev", "sync"}); code != exitOK {
		t.Fatalf("dev sync (null policy) exit = %d (stderr=%s)", code, errb.String())
	}
	if _, err := os.Stat(bundlePath); err != nil {
		t.Fatalf("allow/no-policy bundle not written: %v", err)
	}
}

func TestDevSync_RawRegoWarnsAndProceeds(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundle.json")
	t.Setenv("OPENBOX_CONFIG", filepath.Join(dir, "none.json"))
	t.Setenv("OPENBOX_CONTROL_TOKEN", "obx_key_x")
	t.Setenv("OPENBOX_BACKEND_URL", "https://b.example")
	t.Setenv("OPENBOX_AGENT_ID", "a")
	t.Setenv("OPENBOX_SIDECAR_BUNDLE", bundlePath)

	a, out, _ := syncApp(&fakePolicyReader{pol: &backend.Policy{ID: "pol-raw", UpdatedAt: "t", HasRawRego: true}})
	if code := a.run([]string{"dev", "sync"}); code != exitOK {
		t.Fatalf("dev sync raw-rego exit non-zero")
	}
	if !strings.Contains(out.String(), "raw rego") && !strings.Contains(out.String(), "fail-open") {
		t.Errorf("expected a non-secret raw-rego warning, got %q", out.String())
	}
}

func TestDevSync_FetchErrorNonZeroWithHint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENBOX_CONFIG", filepath.Join(dir, "none.json"))
	t.Setenv("OPENBOX_CONTROL_TOKEN", "obx_key_x")
	t.Setenv("OPENBOX_BACKEND_URL", "https://b.example")
	t.Setenv("OPENBOX_AGENT_ID", "a")
	t.Setenv("OPENBOX_SIDECAR_BUNDLE", filepath.Join(dir, "bundle.json"))

	a, _, errb := syncApp(&fakePolicyReader{err: &backend.APIError{StatusCode: 403, Body: "forbidden"}})
	if code := a.run([]string{"dev", "sync"}); code != exitError {
		t.Fatalf("dev sync fetch error exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(errb.String(), "read:agent_policy") {
		t.Errorf("expected a mapped 403 hint, got %q", errb.String())
	}
}

func TestDevSync_MissingTokenIsINV1Guard(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENBOX_CONFIG", filepath.Join(dir, "none.json"))
	os.Unsetenv("OPENBOX_CONTROL_TOKEN")
	a, _, errb := syncApp(&fakePolicyReader{})
	if code := a.run([]string{"dev", "sync"}); code != exitError {
		t.Fatalf("missing token exit = %d, want error", code)
	}
	if !strings.Contains(errb.String(), "OPENBOX_CONTROL_TOKEN") {
		t.Errorf("expected the INV-1 token guard, got %q", errb.String())
	}
}

// TestDevInit_PersistsAgentIDAndBackendURL pins §6 / G3-F5: `dev init` persists
// the backend agent_id + backend_url to dev.json (non-secret), they are preserved
// across an idempotent re-init, and ResolveAgentID()/ResolveBackendURL() return
// them with the env unset.
func TestDevInit_PersistsAgentIDAndBackendURL(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "openbox", "dev.json")
	t.Setenv("HOME", home)
	t.Setenv("OPENBOX_CONFIG", cfgPath)
	// Env-unset for the resolvers so we prove the CONFIG fallback carries them.
	t.Setenv("OPENBOX_AGENT_ID", "")
	t.Setenv("OPENBOX_BACKEND_URL", "")

	newApp := func() (*app, *bytes.Buffer) {
		a, _, errb := testApp(map[string]string{
			"OPENBOX_CONTROL_TOKEN": "obx_key_x",
			"OPENBOX_BACKEND_URL":   "https://backend.acme",
		})
		store := secret.NewMemStore()
		a.openStore = func(string) (secret.Store, error) { return store, nil }
		a.newRegistrar = func(_, _, _ string) devinit.Registrar {
			return &fakeReg{reg: &backend.Registration{AgentID: "agent-123", DID: "did:aip:x", APIKey: "obx_test_k", PrivateKey: "c2VlZA=="}}
		}
		// newPolicyReader left nil → the last-step sync is skipped (this test is about
		// persistence, not the fetch).
		return a, errb
	}

	a, errb := newApp()
	if code := a.run([]string{"dev", "init", "--provider", "claude-code", "--org", "acme"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}

	assertPersisted := func(when string) {
		raw, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatalf("%s: read dev config: %v", when, err)
		}
		if !strings.Contains(string(raw), `"agent_id": "agent-123"`) {
			t.Errorf("%s: dev.json missing agent_id:\n%s", when, raw)
		}
		if !strings.Contains(string(raw), `"backend_url": "https://backend.acme"`) {
			t.Errorf("%s: dev.json missing backend_url:\n%s", when, raw)
		}
		// Resolvers read it back with env unset (config fallback).
		if got := claudecode.ResolveAgentID(); got != "agent-123" {
			t.Errorf("%s: ResolveAgentID() = %q, want agent-123", when, got)
		}
		if got := claudecode.ResolveBackendURL(); got != "https://backend.acme" {
			t.Errorf("%s: ResolveBackendURL() = %q, want https://backend.acme", when, got)
		}
	}
	assertPersisted("after init")

	// Idempotent re-init (creds already in a fresh store won't be reused, but the
	// installer must PRESERVE the prior agent_id/backend_url even when the ref does
	// not carry them). Re-run and re-assert.
	a2, errb2 := newApp()
	if code := a2.run([]string{"dev", "init", "--provider", "claude-code", "--org", "acme"}); code != exitOK {
		t.Fatalf("re-init exit = %d; stderr=%q", code, errb2.String())
	}
	assertPersisted("after re-init")
}
