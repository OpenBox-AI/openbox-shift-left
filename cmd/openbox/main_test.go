package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/backend"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/devinit"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

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

func testApp(env map[string]string) (*app, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	a := &app{
		stdout: &out,
		stderr: &errb,
		getenv: func(k string) string { return env[k] },
		newRegistrar: func(_, _, _ string) devinit.Registrar {
			panic("newRegistrar should not be called in this path")
		},
	}
	return a, &out, &errb
}

// isolateHome redirects everything `init` writes to outside its own arguments,
// so a command under test cannot reach the developer's real machine: The cwd
// and HOME dirs are deliberately separate temp dirs, so a caller asserting on
// the contents of the returned directory does not also see a plugin bundle or
// a .claude project tree.
//   - OPENBOX_HOME / OPENBOX_CONFIG; the credential file and dev.json;
//   - HOME; the Claude Code plugin bundle.
//   - The working directory.
func isolateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(devconfig.EnvHome, dir)
	t.Setenv(devconfig.EnvConfigPath, filepath.Join(dir, "dev.json"))
	t.Setenv(devconfig.EnvApproverConfigPath, filepath.Join(dir, "approver.json"))
	t.Setenv("HOME", t.TempDir())

	sinks := t.TempDir()
	for env, path := range map[string]string{
		devconfig.EnvEnforcementFile:    filepath.Join(sinks, "enforcements.jsonl"),
		devconfig.EnvPendingApprovalDir: filepath.Join(sinks, "pending-approvals"),
		"OPENBOX_ADVISORY_FILE":         filepath.Join(sinks, "advisories.jsonl"),
	} {
		if os.Getenv(env) == "" {
			t.Setenv(env, path)
		}
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve working directory: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("isolate working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	return dir
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
	isolateHome(t)
	a, out, _ := testApp(nil) // empty env; the registrar seam panics if touched
	code := a.run([]string{"init", "--provider", "claude-code", "--dry-run"})
	if code != exitOK {
		t.Fatalf("dry-run exit = %d", code)
	}
	if !strings.Contains(out.String(), "DRY RUN") {
		t.Errorf("missing DRY RUN banner: %q", out.String())
	}
}

// `init` no longer needs a control token at all; it makes no control-plane
// call. What it needs is credentials already on the machine, and when they are
// absent it must exit non-zero naming `auth` and install nothing.
func TestInitWithoutCredentialsRefusesAndInstallsNothing(t *testing.T) {
	home := isolateHome(t)
	a, _, errb := testApp(nil)
	code := a.run([]string{"init", "--provider", "claude-code"})
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(errb.String(), "openbox auth") {
		t.Errorf("error should point at `openbox auth`, got %q", errb.String())
	}
	if !strings.Contains(errb.String(), "Nothing was installed") {
		t.Errorf("error should state that nothing was installed, got %q", errb.String())
	}
	if entries, err := os.ReadDir(home); err == nil {
		for _, e := range entries {
			if e.Name() == ".env" {
				t.Error("init created a credential file; it must never write one")
			}
		}
	}
}

// `init` must not accept a control token as a way to register, either: the
// registration path is gone from it entirely.
func TestInitDoesNotRegisterEvenWithAnOrgKey(t *testing.T) {
	isolateHome(t)
	a, _, errb := testApp(map[string]string{"OPENBOX_CONTROL_TOKEN": "obx_key_x"})
	a.newRegistrar = func(_, _, _ string) devinit.Registrar {
		t.Error("init reached the registrar; registration belongs to `openbox auth`")
		return &fakeReg{}
	}
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitError {
		t.Fatalf("exit = %d, want an error (no credentials); stderr=%q", code, errb.String())
	}
}

// What replaces it is the contract below; the flag that selected a backend
// must fail rather than be ignored.

// TestRemovedSecretBackendFlagFailsLoudly a removed flag that is silently
// accepted is worse than one that errors: a script passing --secret-backend
// would keep exiting 0 while storing credentials somewhere it did not choose.
func TestRemovedSecretBackendFlagFailsLoudly(t *testing.T) {
	for _, args := range [][]string{
		{"init", "--provider", "claude-code", "--secret-backend", "file"},
		{"init", "--provider", "claude-code", "--secret-backend", "os"},
		{"init", "--role", "approver", "--org", "acme", "--backend-url", "https://x", "--secret-backend", "file"}, // approver still takes --org/--backend-url
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolateHome(t)
			a, _, errb := testApp(map[string]string{"OPENBOX_CONTROL_TOKEN": "obx_key_x", "OPENBOX_BACKEND_URL": "https://x"})
			if code := a.run(args); code != exitError {
				t.Fatalf("exit = %d, want %d — a removed flag must not be silently accepted", code, exitError)
			}
			if !strings.Contains(errb.String(), "openbox auth") {
				t.Errorf("error should point at `openbox auth`, got %q", errb.String())
			}
		})
	}
}

// TestConfigManualOnlyExitsTwo a provider whose adapter is not built is a
// partial success worth its own exit code, so a script can tell it apart from
// a hard failure.
func TestConfigManualOnlyExitsTwo(t *testing.T) {
	home := isolateHome(t)
	seedCredentials(t)
	a, _, errb := testApp(nil)
	code := a.run([]string{"init", "--provider", "cursor"})
	if code != exitConfigOnly {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitConfigOnly, errb.String())
	}
	if !strings.Contains(errb.String(), "note:") {
		t.Errorf("expected a note on partial success, got %q", errb.String())
	}
	kv, err := devconfig.ParseEnvFile(filepath.Join(home, ".env"))
	if err != nil {
		t.Fatalf("read credential file: %v", err)
	}
	if kv[devconfig.EnvAPIKeyDirect] != "obx_test_k" || kv[devconfig.EnvAgentPrivateKey] != testSeedB64 {
		t.Errorf("init modified the credential file: %v", kv)
	}
}

// TestClaudeCodeInstallsForRealExitsZero proves the SL4-wire-1 front door: the
// CLI registers the real claudecode.Installer (not the SL-2 stub), so `init
// --provider claude-code` materializes the plugin bundle + dev config and
// exits 0.
func TestClaudeCodeInstallsForRealExitsZero(t *testing.T) {
	isolateHome(t)
	home := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "openbox", "dev.json")
	t.Setenv("HOME", home)
	t.Setenv("OPENBOX_CONFIG", cfgPath)

	seedCredentials(t)
	a, out, errb := testApp(nil)

	code := a.run([]string{"init", "--provider", "claude-code"})
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitOK, errb.String())
	}
	if !strings.Contains(out.String(), "Wrote claude-code native config") {
		t.Errorf("expected a config-applied message, got %q", out.String())
	}

	if _, err := os.Stat(filepath.Join(home, ".claude", "plugins", "openbox-observe", ".claude-plugin", "plugin.json")); err != nil {
		t.Errorf("plugin bundle not materialized: %v", err)
	}
	enginePath := filepath.Join(home, ".claude", "plugins", "openbox-observe", "bin", "openbox")
	if fi, err := os.Stat(enginePath); err != nil {
		t.Errorf("engine not placed at bin/openbox via init: %v", err)
	} else if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("placed engine is not executable: %v", fi.Mode())
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read dev config: %v", err)
	}
	if strings.Contains(string(raw), "obx_test_k") || strings.Contains(string(raw), "c2VlZA==") {
		t.Errorf("dev config leaked a secret value:\n%s", raw)
	}
	envPath, err := devconfig.EnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	kv, err := devconfig.ParseEnvFile(envPath)
	if err != nil {
		t.Fatalf("read credential file: %v", err)
	}
	if kv[devconfig.EnvAPIKeyDirect] != "obx_test_k" || kv[devconfig.EnvAgentPrivateKey] != testSeedB64 {
		t.Errorf("init modified the credential file: %v", kv)
	}
}

// TestClaudeCodeDryRunWritesNothing a claude-code --dry-run must render the
// real installer's plan but write nothing to disk (no bundle, no config), even
// though claude-code is now a real installer.
func TestClaudeCodeDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "openbox", "dev.json")
	t.Setenv("HOME", home)
	t.Setenv("OPENBOX_CONFIG", cfgPath)

	a, out, _ := testApp(nil) // no token/store/registrar: dry-run must stay offline
	code := a.run([]string{"init", "--provider", "claude-code", "--dry-run"})
	if code != exitOK {
		t.Fatalf("dry-run exit = %d", code)
	}
	if !strings.Contains(out.String(), "OpenBox Claude Code plugin (observe-only") {
		t.Errorf("dry-run did not render the real installer plan:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Errorf("dry-run created a plugin dir under HOME (err=%v)", err)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("dry-run wrote the dev config (err=%v)", err)
	}
}

func setHookEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	t.Setenv("OPENBOX_AGENT_DID", "did:aip:7f3c9b2e-0000-5000-a000-000000000001")
	t.Setenv("OPENBOX_SPOOL_DIR", spool)
	t.Setenv("OPENBOX_SESSION_DIR", filepath.Join(dir, "sessions"))
	t.Setenv("OPENBOX_CONFIG", filepath.Join(dir, "none.json"))
	t.Setenv(devconfig.EnvEnforcementFile, filepath.Join(dir, "enforcements.jsonl"))
	t.Setenv(devconfig.EnvPendingApprovalDir, filepath.Join(dir, "pending-approvals"))
	t.Setenv("OPENBOX_ADVISORY_FILE", filepath.Join(dir, "advisories.jsonl"))
	// Content capture defaults ON, and since that decision the observe path
	// carries the tool's input under that gate; so a hook test that left the
	// posture to the default would silently start asserting the capture-ON
	// behaviour.
	t.Setenv(devconfig.EnvContentCapture, "0")
	return spool
}

// TestHookIsObserveOnlyInProcess drives the unified subcommand in-process and
// asserts the INV-3 contract: exit 0, empty stdout, event spooled, no content
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
		t.Fatalf("command content leaked into the spool with content capture OFF: %s", raw)
	}
}

// TestHookMisuseIsSafe: a bad/missing provider or event still exits 0 with
// empty stdout (never block, never inject).
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
// exit-0/empty-stdout contract must survive folding the hook into the multi-
// command `openbox` binary.
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
		"OPENBOX_HOME="+dir,
		devconfig.EnvEnforcementFile+"="+filepath.Join(dir, "enforcements.jsonl"),
		devconfig.EnvPendingApprovalDir+"="+filepath.Join(dir, "pending-approvals"),
		"OPENBOX_ADVISORY_FILE="+filepath.Join(dir, "advisories.jsonl"),
		"OPENBOX_SESSION_DIR="+filepath.Join(dir, "sessions"),
		devconfig.EnvContentCapture+"=0",
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
		t.Fatalf("content leaked into the spool with content capture OFF: %s", raw)
	}
}

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

// TestHookEndToEndSmoke drives all five hooks through the unified subcommand
// and asserts the whole observe→deliver path on the unified binary (in-
// process):
//   - The HOT-PATH hooks (SessionStart..PostToolUse) never block on the
//     network; they spool locally and cause zero egress; delivery happens only
//     at SessionEnd (AC4 latency budget: the async/no-network-on-hot-path
//     guarantee);
//   - Each hot-path hook returns well within a coarse wall-clock budget;
//   - SessionEnd flushes the spooled session to /evaluate and drains the
//     spool;
func TestHookEndToEndSmoke(t *testing.T) {
	const contentCanary = "SECRET-EGRESS-CANARY-do-not-send"
	var mu sync.Mutex
	got := 0
	var bodies []string
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Setenv(devconfig.EnvEnforcementFile, filepath.Join(dir, "enforcements.jsonl"))
	t.Setenv(devconfig.EnvPendingApprovalDir, filepath.Join(dir, "pending-approvals"))
	t.Setenv("OPENBOX_ADVISORY_FILE", filepath.Join(dir, "advisories.jsonl"))
	// Realtime-on delivery has its own binary-driven test
	// (TestHookRealtimeDelivery); it cannot be exercised in-process, because the
	// trigger refuses to spawn a `*.test` binary.
	t.Setenv("OPENBOX_REALTIME", "0")
	// The canary proves no tool content reaches the wire.
	t.Setenv("OPENBOX_CONTENT_CAPTURE", "0")

	// Every hot-path hook must be fast; only the non-gating ones must be silent.
	events := []struct {
		hook, payload string
		hotPath       bool // must be fast
		gating        bool // egresses synchronously by design
	}{
		{"SessionStart", `{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/r","source":"startup"}`, true, false},
		{"UserPromptSubmit", `{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"/r","prompt":"hi"}`, true, true},
		{"PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"s1","cwd":"/r","tool_name":"Bash","tool_input":{"command":"` + contentCanary + `"}}`, true, true},
		{"PostToolUse", `{"hook_event_name":"PostToolUse","session_id":"s1","cwd":"/r","tool_name":"Bash","tool_response":{"ok":true}}`, true, false},
		{"SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"s1","cwd":"/r","reason":"other"}`, false, false},
	}
	const hotPathBudget = 2 * time.Second
	for _, e := range events {
		a, out, errb := testApp(nil)
		a.stdin = strings.NewReader(e.payload)
		mu.Lock()
		before := got
		mu.Unlock()
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
			if !e.gating {
				mu.Lock()
				n := got - before
				mu.Unlock()
				if n != 0 {
					t.Fatalf("non-gating hot-path hook %s caused egress (%d /evaluate calls) — "+
						"only the gate may block on the network (NFR-2)", e.hook, n)
				}
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
	drained, _ := os.ReadDir(spool)
	for _, e := range drained {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			t.Errorf("spool not drained after SessionEnd flush: %s", e.Name())
		}
	}
	for i, b := range delivered {
		if strings.Contains(b, contentCanary) {
			t.Fatalf("content canary leaked to /evaluate in delivered body #%d:\n%s", i, b)
		}
	}
	all := strings.Join(delivered, "\n")
	if !strings.Contains(all, `"activity_type":"Bash"`) {
		t.Errorf("no delivered body carried activity_type=Bash (tool label lost across spool):\n%s", all)
	}
	if !strings.Contains(all, `"activity_type":"SessionStarted"`) {
		t.Errorf("no delivered body carried activity_type=SessionStarted (lifecycle label):\n%s", all)
	}
}

// TestHookRealtimeDelivery proves the near-real-time path end-to-end on the
// real binary (the trigger refuses to spawn a `*.test` binary, so this cannot
// run in-process): a hook spools its event and a detached, debounced flusher
// delivers it to /evaluate mid-session; no SessionEnd involved; and the
// SessionEnd that follows delivers exactly the remainder (no loss, no
// duplicate Idempotency-Keys when realtime and teardown flushes overlap).
func TestHookRealtimeDelivery(t *testing.T) {
	memhttptest.RequireBind(t)
	if testing.Short() {
		t.Skip("builds a binary + spawns detached flushers; skipped in -short")
	}
	var mu sync.Mutex
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/governance/evaluate" {
			_, _ = io.Copy(io.Discard, r.Body)
			mu.Lock()
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"governance_event_id":"ge","verdict":"allow","risk_score":0.1,"action":"continue","fallback_used":false}`))
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	dir := t.TempDir()
	bin := filepath.Join(dir, "openbox")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build openbox: %v\n%s", err, out)
	}
	spool := filepath.Join(dir, "spool")
	env := append(os.Environ(),
		"OPENBOX_AGENT_DID=did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		"OPENBOX_SPOOL_DIR="+spool,
		"OPENBOX_CONFIG="+filepath.Join(dir, "none.json"),
		"OPENBOX_HOME="+dir,
		devconfig.EnvEnforcementFile+"="+filepath.Join(dir, "enforcements.jsonl"),
		devconfig.EnvPendingApprovalDir+"="+filepath.Join(dir, "pending-approvals"),
		"OPENBOX_ADVISORY_FILE="+filepath.Join(dir, "advisories.jsonl"),
		"OPENBOX_SESSION_DIR="+filepath.Join(dir, "sessions"),
		"OPENBOX_BASE_URL="+srv.URL,
		"OPENBOX_API_KEY=obx_test_"+strings.Repeat("a", 48),
		"OPENBOX_ED25519_SEED=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
	)
	runHook := func(hook, payload string) {
		t.Helper()
		cmd := exec.Command(bin, "hook", "claude-code", hook)
		cmd.Stdin = strings.NewReader(payload)
		cmd.Env = env
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s exit != 0: %v\nstderr: %s", hook, err, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("%s wrote to stdout: %q", hook, stdout.String())
		}
	}
	received := func() int { mu.Lock(); defer mu.Unlock(); return len(keys) }
	waitFor := func(desc string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s (received %d)", desc, received())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	runHook("PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"rt","cwd":"/r","tool_name":"Bash","tool_input":{"command":"ls"}}`)
	waitFor("mid-session delivery of the first event", func() bool { return received() >= 1 })

	lock := filepath.Join(spool, "rt.flushlock")
	waitFor("debounce lock release", func() bool { _, err := os.Stat(lock); return os.IsNotExist(err) })
	runHook("PostToolUse", `{"hook_event_name":"PostToolUse","session_id":"rt","cwd":"/r","tool_name":"Bash","tool_response":{"ok":true}}`)
	waitFor("mid-session delivery of the second event", func() bool { return received() >= 2 })

	runHook("SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"rt","cwd":"/r","reason":"other"}`)
	waitFor("SessionEnd delivery", func() bool { return received() >= 3 })
	waitFor("spool drained", func() bool {
		entries, err := os.ReadDir(spool)
		if err != nil {
			return false
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
				return false
			}
		}
		return true
	})
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 3 {
		t.Errorf("want exactly 3 deliveries (Pre, Post, SessionEnd), got %d", len(keys))
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if k == "" {
			t.Error("a delivery carried no Idempotency-Key")
		}
		if seen[k] {
			t.Errorf("duplicate Idempotency-Key %q — an event was double-sent", k)
		}
		seen[k] = true
	}
}

// TestUnifiedBinaryGitHookStampsCommit proves the od17 git-hook fold end-to-
// end: `openbox hook git install` writes a prepare-commit-msg hook that re-
// invokes the unified binary as `openbox hook git prepare-commit-msg`, and a
// real commit gets the OpenBox-Session trailer stamped; with no separate
// openbox-git-hook binary.
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

const verifyTestSeed = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

const verifyTestDID = "did:aip:00000000-0000-0000-0000-000000000042"

func coreValidateOK(t *testing.T, seedB64 string) *memhttptest.Server {
	t.Helper()
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil {
		t.Fatalf("decode seed: %v", err)
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func setVerifyCreds(t *testing.T, baseURL string) {
	t.Helper()
	t.Setenv("OPENBOX_CONFIG", filepath.Join(t.TempDir(), "none.json"))
	t.Setenv("OPENBOX_BASE_URL", baseURL)
	t.Setenv("OPENBOX_AGENT_DID", verifyTestDID)
	t.Setenv("OPENBOX_API_KEY", "obx_test_"+strings.Repeat("a", 48))
	t.Setenv("OPENBOX_ED25519_SEED", verifyTestSeed)
}

// TestDevVerifyHappyPath: a valid key + signing round-trip against the mock
// core prints a ✓ line naming the DID + base_url and exits 0.
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
	if strings.Contains(out.String(), verifyTestSeed) || strings.Contains(out.String(), "obx_test_") {
		t.Errorf("INV-1: secret leaked into ✓ output: %q", out.String())
	}
}

// TestDevVerifyBadKeyIsMappedFailure: core rejects the identity (401) → a ✗
// with the mapped fix hint on stderr and a non-zero exit; no secret leaks.
func TestDevVerifyBadKeyIsMappedFailure(t *testing.T) {
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
// base_url, DID) and makes NO network call; the registrar/store seams panic if
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
	a2, out2, _ := testApp(nil)
	if code := a2.run([]string{"dev", "verify", "--print-plan"}); code != exitOK {
		t.Fatalf("--print-plan exit = %d", code)
	}
	if !strings.Contains(out2.String(), "DRY RUN") {
		t.Errorf("--print-plan did not render the plan: %q", out2.String())
	}
}

// TestDevVerifyNoCredsSaysInitFirst: with nothing configured, verify exits
// non-zero and tells the operator to run `openbox init` (never half-proceeds).
func TestDevVerifyNoCredsSaysInitFirst(t *testing.T) {
	t.Setenv("OPENBOX_CONFIG", filepath.Join(t.TempDir(), "none.json"))
	for _, k := range []string{"OPENBOX_BASE_URL", "OPENBOX_AGENT_DID", "OPENBOX_API_KEY", "OPENBOX_ED25519_SEED", "OPENBOX_SECRET_FILE"} {
		t.Setenv(k, "")
	}
	a, _, errb := testApp(nil)
	code := a.run([]string{"dev", "verify"})
	if code != exitError {
		t.Fatalf("no-creds exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(errb.String(), "openbox init") {
		t.Errorf("expected a 'run openbox init' hint, got %q", errb.String())
	}
}

func TestUnknownProviderAndMissingProvider(t *testing.T) {
	a, _, _ := testApp(nil)
	if code := a.run([]string{"init", "--provider", "vim", "--dry-run"}); code != exitError {
		t.Errorf("unknown provider exit = %d", code)
	}
	if code := a.run([]string{"init", "--dry-run"}); code != exitError {
		t.Errorf("missing provider exit = %d", code)
	}
}

// TestAuth_PersistsAgentIDAndBackendURL coordinate persistence moved from
// `init` to `auth` : `auth` writes agent_id / backend_url / base_url to
// dev.json, they survive a re-run, and the resolvers read them back with the
// environment unset.
func TestAuth_PersistsAgentIDAndBackendURL(t *testing.T) {
	home := isolateHome(t)
	t.Setenv("OPENBOX_AGENT_ID", "")
	t.Setenv("OPENBOX_BACKEND_URL", "")

	run := func(when string) {
		t.Helper()
		a, _, errb := testApp(nil)
		a.stdin = strings.NewReader("obx_test_k\n" + testSeedB64 + "\n")
		code := a.run([]string{"auth", "--api-key-stdin", "--private-key-stdin", "--yes",
			"--agent-id", "agent-123", "--did", "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301",
			"--backend-url", "https://backend.acme"})
		if code != exitOK {
			t.Fatalf("%s: auth exit = %d; stderr=%q", when, code, errb.String())
		}
		raw, err := os.ReadFile(filepath.Join(home, "dev.json"))
		if err != nil {
			t.Fatalf("%s: read dev config: %v", when, err)
		}
		if !strings.Contains(string(raw), `"agent_id": "agent-123"`) {
			t.Errorf("%s: dev.json missing agent_id:\n%s", when, raw)
		}
		if !strings.Contains(string(raw), `"backend_url": "https://backend.acme"`) {
			t.Errorf("%s: dev.json missing backend_url:\n%s", when, raw)
		}
		if got := devconfig.ResolveAgentID(); got != "agent-123" {
			t.Errorf("%s: ResolveAgentID() = %q, want agent-123", when, got)
		}
		if got := devconfig.ResolveBackendURL(); got != "https://backend.acme" {
			t.Errorf("%s: ResolveBackendURL() = %q, want https://backend.acme", when, got)
		}
	}
	run("after auth")
	run("after re-auth")
}

// TestAuth_PersistsBaseURLForASelfHostedCore a self-hosted install has to be
// able to name its own core.
func TestAuth_PersistsBaseURLForASelfHostedCore(t *testing.T) {
	run := func(t *testing.T, env map[string]string, args ...string) string {
		t.Helper()
		home := isolateHome(t)
		t.Setenv("OPENBOX_BASE_URL", "")
		for k, v := range env {
			t.Setenv(k, v)
		}
		a, _, errb := testApp(env)
		a.stdin = strings.NewReader("obx_test_k\n" + testSeedB64 + "\n")
		base := append([]string{"auth", "--api-key-stdin", "--private-key-stdin", "--yes",
			"--did", "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301"}, args...)
		if code := a.run(base); code != exitOK {
			t.Fatalf("auth exit = %d; stderr=%q", code, errb.String())
		}
		raw, err := os.ReadFile(filepath.Join(home, "dev.json"))
		if err != nil {
			t.Fatalf("read dev config: %v", err)
		}
		return string(raw)
	}

	cfg := run(t, nil, "--base-url", "http://localhost:8086")
	if !strings.Contains(cfg, `"base_url": "http://localhost:8086"`) {
		t.Errorf("--base-url was not persisted:\n%s", cfg)
	}
	if got, _ := devconfig.ResolveCoordinates(); got != "http://localhost:8086" {
		t.Errorf("ResolveCoordinates() base = %q, want the self-hosted core", got)
	}

	if cfg := run(t, map[string]string{"OPENBOX_BASE_URL": "http://core.internal:8086"}); !strings.Contains(cfg, `"base_url": "http://core.internal:8086"`) {
		t.Errorf("OPENBOX_BASE_URL was not persisted:\n%s", cfg)
	}

	cfg = run(t, nil)
	if !strings.Contains(cfg, `"base_url": "`+devconfig.DefaultBaseURL+`"`) {
		t.Errorf("the hosted core default was not persisted:\n%s", cfg)
	}
	if !strings.Contains(cfg, `"backend_url": "`+devconfig.DefaultBackendURL+`"`) {
		t.Errorf("the hosted backend default was not persisted:\n%s", cfg)
	}
}

func setCodexHookEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	t.Setenv("OPENBOX_AGENT_DID", "did:aip:7f3c9b2e-0000-5000-a000-000000000001")
	t.Setenv("OPENBOX_SPOOL_DIR", spool)
	t.Setenv("OPENBOX_CONFIG", filepath.Join(dir, "none.json"))
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex-home"))
	t.Setenv("OPENBOX_ADVISORY_FILE", filepath.Join(dir, "advisories.jsonl"))
	t.Setenv("OPENBOX_FINDINGS_CURSOR", filepath.Join(dir, "findings.cursor"))
	t.Setenv("OPENBOX_ENFORCEMENT_FILE", filepath.Join(dir, "enforcements.jsonl"))
	return spool
}

// TestCodexHookIsObserveOnlyInProcess mirrors the claude-code routing test for
// the new provider: exit 0, empty stdout (Codex parses hook stdout as output
// JSON), event spooled, no tool_input content in the spool (SL3-SEC-3).
func TestCodexHookIsObserveOnlyInProcess(t *testing.T) {
	spool := setCodexHookEnv(t)
	a, out, errb := testApp(nil)
	secret := "TOP-SECRET-do-not-egress"
	a.stdin = strings.NewReader(`{"hook_event_name":"PreToolUse","session_id":"th-1","cwd":"/r","tool_name":"Bash","tool_use_id":"call-1","tool_input":{"command":"` + secret + `"}}`)

	code := a.run([]string{"hook", "codex", "PreToolUse"})
	if code != exitOK {
		t.Fatalf("hook exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout must be empty (Codex hook-output parser / no block), got %q", out.String())
	}
	raw, _ := os.ReadFile(filepath.Join(spool, onlySpoolFile(t, spool)))
	if !strings.Contains(string(raw), "ToolCall") {
		t.Errorf("spooled event should be a ToolCall: %s", raw)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("command content leaked into the spool: %s", raw)
	}
}

// TestCodexHookMisuseIsSafe: bad/missing event still exits 0 with empty
// stdout.
func TestCodexHookMisuseIsSafe(t *testing.T) {
	setCodexHookEnv(t)
	for _, args := range [][]string{
		{"hook", "codex"},
		{"hook", "codex", "Stop"}, // real Codex event, deliberately unwired
	} {
		a, out, _ := testApp(nil)
		a.stdin = strings.NewReader("")
		if code := a.run(args); code != exitOK {
			t.Errorf("%v exit = %d, want 0", args, code)
		}
		if out.Len() != 0 {
			t.Errorf("%v wrote to stdout: %q", args, out.String())
		}
	}
}

// TestCodexUnifiedBinaryObserveE2E is the story's real-binary observe E2E
// (AC-10): build the actual `openbox` binary and drive ALL five wired events
// through `openbox hook codex <event>` with the v0.145.0-shaped fixture
// payloads from internal/adapters/codex/testdata.
func TestCodexUnifiedBinaryObserveE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "openbox")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build openbox: %v\n%s", err, out)
	}
	spool := filepath.Join(dir, "spool")
	fixtures := filepath.Join("..", "..", "internal", "adapters", "codex", "testdata")
	env := append(os.Environ(),
		"OPENBOX_AGENT_DID=did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		"OPENBOX_SPOOL_DIR="+spool,
		"OPENBOX_CONFIG="+filepath.Join(dir, "none.json"),
		"OPENBOX_HOME="+dir,
		"CODEX_HOME="+filepath.Join(dir, "codex-home"),
		"OPENBOX_ADVISORY_FILE="+filepath.Join(dir, "advisories.jsonl"),
		"OPENBOX_FINDINGS_CURSOR="+filepath.Join(dir, "findings.cursor"),
		"OPENBOX_ENFORCEMENT_FILE="+filepath.Join(dir, "enforcements.jsonl"),
	)

	for _, e := range []struct{ hook, fixture string }{
		{"SessionStart", "sessionstart.json"},
		{"UserPromptSubmit", "userpromptsubmit.json"},
		{"PreToolUse", "pretooluse.json"},
		{"PostToolUse", "posttooluse.json"},
		{"SessionEnd", "sessionend.json"},
	} {
		payload, err := os.ReadFile(filepath.Join(fixtures, e.fixture))
		if err != nil {
			t.Fatalf("fixture %s: %v", e.fixture, err)
		}
		cmd := exec.Command(bin, "hook", "codex", e.hook)
		cmd.Stdin = bytes.NewReader(payload)
		cmd.Env = env
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s must exit 0 (observe-only), got %v\nstderr: %s", e.hook, err, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("%s stdout must be EMPTY, got %q", e.hook, stdout.String())
		}
	}

	spoolFile := onlySpoolFile(t, spool)
	raw, _ := os.ReadFile(filepath.Join(spool, spoolFile))
	for _, wantType := range []string{"SessionStarted", "PromptSubmitted", "ToolCall", "ToolResult", "SessionEnded"} {
		if !strings.Contains(string(raw), wantType) {
			t.Errorf("spool missing a %s event:\n%s", wantType, raw)
		}
	}
	for _, secret := range []string{"go test ./...", "0.412s"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("tool content leaked into the spool: %s", raw)
		}
	}
}

// TestCodexInstallsForRealExitsZero proves the story-SL7-A registry swap
// through the real `init` front door: the CLI registers the real
// codex.Installer, so `init --provider codex` writes hooks.json (under the
// redirected CODEX_HOME) + the dev config, surfaces the /hooks trust step, and
// exits 0.
func TestCodexInstallsForRealExitsZero(t *testing.T) {
	openboxHome := isolateHome(t)
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	cfgPath := filepath.Join(t.TempDir(), "openbox", "dev.json")
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("OPENBOX_CONFIG", cfgPath)

	seedCredentials(t)
	a, out, errb := testApp(nil)

	code := a.run([]string{"init", "--provider", "codex"})
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitOK, errb.String())
	}
	if !strings.Contains(out.String(), "Wrote codex native config") {
		t.Errorf("expected a config-applied message, got %q", out.String())
	}
	if !strings.Contains(out.String(), "/hooks") {
		t.Errorf("expected the /hooks trust step in the output, got %q", out.String())
	}

	rawHooks, err := os.ReadFile(filepath.Join(codexHome, "hooks.json"))
	if err != nil {
		t.Fatalf("hooks.json not written under CODEX_HOME: %v", err)
	}
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionEnd"} {
		if !strings.Contains(string(rawHooks), "hook codex "+ev) {
			t.Errorf("hooks.json missing the %s entry:\n%s", ev, rawHooks)
		}
	}
	for _, banned := range []string{"obx_", "did:aip:x", "https://x"} {
		if strings.Contains(string(rawHooks), banned) {
			t.Errorf("hooks.json must not carry %q:\n%s", banned, rawHooks)
		}
	}
	rawCfg, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(rawCfg), "obx_test_k") || strings.Contains(string(rawCfg), "c2VlZA==") {
		t.Errorf("dev config leaked a secret value:\n%s", rawCfg)
	}
	kv, err := devconfig.ParseEnvFile(filepath.Join(openboxHome, ".env"))
	if err != nil {
		t.Fatalf("read credential file: %v", err)
	}
	if kv[devconfig.EnvAPIKeyDirect] != "obx_test_k" || kv[devconfig.EnvAgentPrivateKey] != testSeedB64 {
		t.Errorf("init modified the credential file: %v", kv)
	}
}

// TestCodexDryRunWritesNothing a codex --dry-run renders the real installer's
// plan (incl. The trust step) but writes nothing; no hooks.json under
// CODEX_HOME, no dev config.
func TestCodexDryRunWritesNothing(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	cfgPath := filepath.Join(t.TempDir(), "openbox", "dev.json")
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("OPENBOX_CONFIG", cfgPath)

	a, out, _ := testApp(nil) // no token/store/registrar: dry-run must stay offline
	code := a.run([]string{"init", "--provider", "codex", "--dry-run"})
	if code != exitOK {
		t.Fatalf("dry-run exit = %d", code)
	}
	for _, want := range []string{"OpenBox Codex hooks", "/hooks", "hook codex"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry-run plan missing %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(codexHome); !os.IsNotExist(err) {
		t.Errorf("dry-run created CODEX_HOME content (err=%v)", err)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("dry-run wrote the dev config (err=%v)", err)
	}
}

// TestDevSyncIsRetired asking for help is not an error. A removed command must
// SAY it was removed.
func TestDevSyncIsRetired(t *testing.T) {
	a, _, errb := testApp(nil)
	if code := a.run([]string{"dev", "sync"}); code == exitOK {
		t.Error("`dev sync` exited 0 — a retired command must fail, not no-op")
	}
	for _, want := range []string{"no longer exists", "evaluated by OpenBox", "inert"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("stderr %q must mention %q, so an operator learns what replaced it and "+
				"that the leftover bundle file on disk is harmless", errb.String(), want)
		}
	}
}

func TestHelpFlagExitsZeroForEverySubcommand(t *testing.T) {
	for _, args := range [][]string{
		{"init", "-h"},
		{"dev", "verify", "-h"},
		{"doctor", "-h"},
		{"managed", "-h"},
	} {
		a, _, _ := testApp(nil)
		if got := a.run(args); got != exitOK {
			t.Errorf("openbox %v exited %d, want 0 — asking for help is not an error", args, got)
		}
	}
}

// TestUnknownFlagExitsNonZero a parse error is still an error.
func TestUnknownFlagExitsNonZero(t *testing.T) {
	a, _, _ := testApp(nil)
	if got := a.run([]string{"dev", "verify", "--no-such-flag"}); got == exitOK {
		t.Error("an unknown flag must not exit 0")
	}
}

// TestInit_SaysWhichSessionsAreGoverned an install that reports success and
// governs nothing is the worst outcome available to an onboarding flow: the
// config is correct, the exit code is 0, and the first evidence of the gap is
// an empty dashboard, which reads as a broken product rather than an
// unfinished rollout.
func TestInit_SaysWhichSessionsAreGoverned(t *testing.T) {
	run := func(t *testing.T, extra ...string) string {
		t.Helper()
		isolateHome(t)
		seedCredentials(t)
		t.Setenv("OPENBOX_AGENT_ID", "")

		a, out, errb := testApp(nil)
		args := append([]string{"init", "--provider", "claude-code"}, extra...)
		if code := a.run(args); code != exitOK {
			t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
		}
		return out.String()
	}

	t.Run("the default governs this project and states the gap", func(t *testing.T) {
		got := run(t)
		if !strings.Contains(got, "THIS PROJECT ONLY") {
			t.Errorf("a bare init governs the current directory and must say so; got:\n%s", got)
		}
		if !strings.Contains(got, "settings.local.json") {
			t.Errorf("say WHERE the hooks were written so it can be checked; got:\n%s", got)
		}
		if !strings.Contains(got, "ANY OTHER directory are not governed") {
			t.Errorf("the default must state what it does NOT cover; got:\n%s", got)
		}
		if !strings.Contains(got, "absence of events is not evidence") {
			t.Errorf("the coverage gap must be stated in the terms an auditor needs; got:\n%s", got)
		}
		if strings.Contains(got, "NOTHING YET") {
			t.Errorf("contradictory output: claims both governed and ungoverned:\n%s", got)
		}
	})

	t.Run("global scope says activation is pending and touches no project file", func(t *testing.T) {
		dir := t.TempDir()
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(wd) })

		got := run(t, "--scope", "global")
		if !strings.Contains(got, "NOTHING YET") {
			t.Errorf("global scope must say it governs nothing yet; got:\n%s", got)
		}
		for _, want := range []string{"managed settings", "enabledPlugins", "--scope local"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing the remedy %q; got:\n%s", want, got)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, ".claude", "settings.local.json")); !os.IsNotExist(err) {
			t.Errorf("--scope global wrote a project settings file: %v", err)
		}
	})
}

func seedCredentials(t *testing.T) {
	t.Helper()
	envPath, err := devconfig.EnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteEnvFile(envPath, map[string]string{
		devconfig.EnvAPIKeyDirect:    "obx_test_k",
		devconfig.EnvAgentPrivateKey: testSeedB64,
	}); err != nil {
		t.Fatal(err)
	}
	devPath, err := devconfig.DevConfigWritePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteConfig(devPath, devconfig.Update{
		DID: "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301",
	}); err != nil {
		t.Fatal(err)
	}
}
