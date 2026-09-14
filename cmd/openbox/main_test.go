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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/backend"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/devinit"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/prompt"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
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
		// Non-interactive by default. A test that did not script answers is a
		// test about something other than prompting, and this is the shape those
		// runs take in production -- a CI machine with no terminal.
		newPrompt: func() (prompt.Prompter, error) { return nil, prompt.ErrNotATerminal }}
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
	dir := isolateHomeOnly(t)
	t.Setenv(devconfig.EnvConfigPath, filepath.Join(dir, "dev.json"))
	return dir
}

// isolateHomeOnly is isolateHome without the OPENBOX_CONFIG pin, for a test
// that needs each tool's dev.json to land in that tool's own directory. With
// the pin set, two tools share one dev.json and a per-tool test passes
// vacuously against the override -- which is the whole failure it exists to
// catch, so the pin is cleared rather than merely left alone.
//
// It binds claude-code, because that is what the command under test will bind
// and a fixture that seeds a different store than the command reads proves
// nothing. A test that wants another tool rebinds over this one.
func isolateHomeOnly(t *testing.T) string {
	t.Helper()
	dir := isolateHomeUnbound(t)
	bindProviderForTest(t, defaultTestProvider)
	return dir
}

// isolateHomeUnbound is isolateHomeOnly with nothing bound, for the one case
// that has to look like a real process before it learns which tool it is
// acting for: a git hook with no tool marker in its environment.
func isolateHomeUnbound(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(devconfig.EnvHome, dir)
	t.Setenv(devconfig.EnvConfigPath, "")
	t.Setenv("HOME", t.TempDir())

	sinks := t.TempDir()
	for env, path := range map[string]string{
		devconfig.EnvEnforcementFile:    filepath.Join(sinks, "enforcements.jsonl"),
		devconfig.EnvPendingApprovalDir: filepath.Join(sinks, "pending-approvals"),
		"OPENBOX_ADVISORY_FILE":         filepath.Join(sinks, "advisories.jsonl")} {
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
	fakeSupervisor(t, dir)
	return dir
}

// defaultTestProvider is the tool a fixture binds and seeds when a test does
// not say otherwise.
const defaultTestProvider = "claude-code"

// bindProviderForTest binds for the length of one test. A test binary
// dispatches many commands in one process, so the release is mandatory:
// without it the first bind would decide every later case's identity. Nothing
// that binds may call t.Parallel().
func bindProviderForTest(t *testing.T, tool string) {
	t.Helper()
	release, err := devconfig.BindProvider(tool)
	if err != nil {
		t.Fatalf("bind %s: %v", tool, err)
	}
	t.Cleanup(release)
}

// fakeSupervisor replaces every seam that leaves this process, and seeds the
// transport CA so a default install completes.
//
// It belongs here rather than in the tests that ask for it, because `init` now
// installs the lanes unconditionally: every one of the ~30 init invocations in
// this package would otherwise hand a unit naming the TEST BINARY to the real
// launchd, with KeepAlive and Restart=always, and dial the developer's own
// lanes to decide whether a port was free. TestMain refuses those seams by
// default so the omission cannot be silent; this is what makes the default
// path work.
func fakeSupervisor(t *testing.T, openboxHome string) {
	t.Helper()
	origRun, origUID := run, currentUID
	origProbe, origListen, origFree := portOccupied, waitForListenerFn, waitForPortFreeFn
	origInstall, origUninstall := installLaneUnitFn, uninstallLaneUnitFn
	origGwInstall, origGwUninstall := installUnitFn, uninstallUnitFn
	t.Cleanup(func() {
		run, currentUID = origRun, origUID
		portOccupied, waitForListenerFn, waitForPortFreeFn = origProbe, origListen, origFree
		installLaneUnitFn, uninstallLaneUnitFn = origInstall, origUninstall
		installUnitFn, uninstallUnitFn = origGwInstall, origGwUninstall
	})

	run = func(string, ...string) error { return nil }
	currentUID = func() string { return "501" }
	// The test binary itself lives under go-build, which selfPath refuses --
	// correctly, since a unit naming it would outlive it. A stable path stands
	// in for the installed binary.
	origExe := executableFn
	t.Cleanup(func() { executableFn = origExe })
	executableFn = func() (string, error) { return "/usr/local/bin/openbox", nil }
	portOccupied = func(string) (bool, string) { return false, "" }
	waitForListenerFn = func(string, time.Duration) bool { return true }
	waitForPortFreeFn = func(string, time.Duration) bool { return true }
	installLaneUnitFn = func(spec laneservice.Spec, goos, homeDir, binPath string) error {
		_, err := spec.WriteUnit(goos, homeDir, binPath)
		return err
	}
	uninstallLaneUnitFn = func(spec laneservice.Spec, goos, homeDir string) error {
		_, err := spec.RemoveUnit(goos, homeDir)
		return err
	}
	installUnitFn = func(goos, homeDir, binPath, addr, upstream string, verbose bool) error {
		_, err := gatewayservice.WriteUnit(goos, homeDir, binPath, addr, upstream, verbose)
		return err
	}
	uninstallUnitFn = func(goos, homeDir string) error {
		_, err := gatewayservice.RemoveUnit(goos, homeDir)
		return err
	}

	// The transport lane refuses to point NODE_EXTRA_CA_CERTS at a CA that is
	// not there, which is correct and would make every default install in this
	// package report a failed lane. The daemon mints this file on first start;
	// with no daemon, the fixture stands in for it.
	if err := os.MkdirAll(openboxHome, 0o700); err != nil {
		t.Fatal(err)
	}
	caPath, _ := transport.CAPaths(openboxHome)
	if err := os.WriteFile(caPath, []byte("-----BEGIN CERTIFICATE-----\nstub\n-----END CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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

// TestInitIsOfflineAndNeedsNoToken. `init` registers an agent for a tool that
// has none, but a tool that already has one must install with an empty
// environment and no control-plane call at all -- that is the re-run, and it
// has to work on a plane. testApp's registrar seam panics if anything reaches
// for one, which is what makes "offline" an assertion rather than a claim.
func TestInitIsOfflineAndNeedsNoToken(t *testing.T) {
	isolateHome(t)
	seedCredentials(t)
	t.Setenv(devconfig.EnvControlToken, "")
	a, out, errb := testApp(nil) // empty env; the registrar seam panics if touched
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "EVERY SESSION") {
		t.Errorf("the install did not complete:\n%s", out.String())
	}
}

// TestInitWithoutCredentialsRefusesAndInstallsNothing the refusal narrowed
// when `init` learned to register: it fires only when the tool has NEITHER a
// store NOR an organization token to make one with. Either alone is now a
// working install, so a test that seeded neither and called that "no
// credentials" would stop distinguishing the refusal from the register branch.
func TestInitWithoutCredentialsRefusesAndInstallsNothing(t *testing.T) {
	home := isolateHome(t)
	t.Setenv(devconfig.EnvControlToken, "")
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

// TestInitRegistersOnlyWhenTheToolHasNoIdentity inverts what this file used to
// assert. `init` registers now, and the question that decides it is the tool's
// own store -- not the environment, not the org file, not another tool. Both
// halves are asserted together because satisfying one by breaking the other is
// the easy mistake: a run that always registers orphans an agent on every
// re-init, and a run that never registers is the state before this work.
func TestInitRegistersOnlyWhenTheToolHasNoIdentity(t *testing.T) {
	t.Run("no store: registers once", func(t *testing.T) {
		isolateHome(t)
		t.Setenv(devconfig.EnvControlToken, testOrgToken)
		clearAgentEnv(t)
		reg := &countingReg{byName: map[string]*backend.AgentSummary{}}
		a, _, errb := testApp(nil)
		a.newRegistrar = func(_, _, _ string) devinit.Registrar { return reg }
		a.newPrompt = declineAdopt(t)
		if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
			t.Fatalf("exit = %d; stderr=%q", code, errb.String())
		}
		if reg.creates != 1 {
			t.Errorf("Create called %d times, want 1", reg.creates)
		}
	})

	t.Run("store present: never reaches the registrar", func(t *testing.T) {
		isolateHome(t)
		seedCredentials(t)
		t.Setenv(devconfig.EnvControlToken, testOrgToken)
		a, _, errb := testApp(nil)
		a.newRegistrar = func(_, _, _ string) devinit.Registrar { return panicReg{} }
		a.newPrompt = panicPrompt(t)
		if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
			t.Fatalf("exit = %d; stderr=%q", code, errb.String())
		}
	})
}

// What replaces it is the contract below; the flag that selected a backend
// must fail rather than be ignored.

// TestRemovedSecretBackendFlagFailsLoudly a removed flag that is silently
// accepted is worse than one that errors: a script passing --secret-backend
// would keep exiting 0 while storing credentials somewhere it did not choose.
//
// --role went with the approver persona, so it is refused the same way rather
// than selecting a second install path.
func TestRemovedSecretBackendFlagFailsLoudly(t *testing.T) {
	for _, args := range [][]string{
		{"init", "--provider", "claude-code", "--secret-backend", "file"},
		{"init", "--provider", "claude-code", "--secret-backend", "os"},
		{"init", "--role", "approver", "--org", "acme"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolateHome(t)
			a, _, errb := testApp(map[string]string{"OPENBOX_CONTROL_TOKEN": "obx_key_x", "OPENBOX_BACKEND_URL": "https://x"})
			if code := a.run(args); code != exitError {
				t.Fatalf("exit = %d, want %d; a removed flag must not be silently accepted", code, exitError)
			}
			// The usage block that prints on refusal points at the two commands
			// that took this work over.
			if !strings.Contains(errb.String(), "openbox auth") {
				t.Errorf("error should point at `openbox auth`, got %q", errb.String())
			}
		})
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

	// The bundle hosts the engine and nothing else. A plugin manifest there
	// would make the directory loadable, and a plugin's handlers do not
	// de-duplicate against the settings-level ones this install writes.
	bundle := filepath.Join(home, ".claude", "plugins", "openbox-observe")
	var manifests []string
	_ = filepath.WalkDir(bundle, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(path) == ".json" {
			manifests = append(manifests, path)
		}
		return nil
	})
	if len(manifests) > 0 {
		t.Errorf("the bundle ships loadable manifest(s), which would double every event: %v", manifests)
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
		{"hook", "vim", "PreToolUse"}} {
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
		{"SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"s1","cwd":"/r","reason":"other"}`, false, false}}
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
				t.Errorf("%s hot-path hook took %v (> budget %v); is it blocking on the network?", e.hook, elapsed, hotPathBudget)
			}
			if !e.gating {
				mu.Lock()
				n := got - before
				mu.Unlock()
				if n != 0 {
					t.Fatalf("non-gating hot-path hook %s caused egress (%d /evaluate calls); "+
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
		t.Fatalf("mock /evaluate received no events; SessionEnd flush did not deliver through the unified binary")
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
			t.Errorf("duplicate Idempotency-Key %q; an event was double-sent", k)
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

// TestAuth_PersistsTheBackendURL coordinate persistence moved from `init` to
// `auth`: `auth` writes backend_url / base_url to the org dev.json, they
// survive a re-run, and the resolvers read them back with the environment
// unset. The agent id came off this list when identity went per tool -- it
// names one tool's agent, and this file is shared by all of them, so `init`
// writes it into the tool's own config instead.
func TestAuth_PersistsTheBackendURL(t *testing.T) {
	home := isolateHome(t)
	t.Setenv("OPENBOX_AGENT_ID", "")
	t.Setenv("OPENBOX_BACKEND_URL", "")

	run := func(when string) {
		t.Helper()
		a, _, errb := testApp(nil)
		// backend, core, org control token.
		scriptedAuth(t, a, "https://backend.acme", "", "obx_key_"+strings.Repeat("f", 48))
		code := a.run([]string{"auth"})
		if code != exitOK {
			t.Fatalf("%s: auth exit = %d; stderr=%q", when, code, errb.String())
		}
		raw, err := os.ReadFile(filepath.Join(home, "dev.json"))
		if err != nil {
			t.Fatalf("%s: read dev config: %v", when, err)
		}
		if !strings.Contains(string(raw), `"backend_url": "https://backend.acme"`) {
			t.Errorf("%s: dev.json missing backend_url:\n%s", when, raw)
		}
		if strings.Contains(string(raw), `"agent_id"`) {
			t.Errorf("%s: auth wrote an agent id into the org config:\n%s", when, raw)
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
	// coreURL is the answer given at the core-URL prompt; blank keeps whatever
	// was prefilled, which is the hosted default on a fresh machine.
	run := func(t *testing.T, env map[string]string, coreURL string) string {
		t.Helper()
		home := isolateHome(t)
		t.Setenv("OPENBOX_BASE_URL", "")
		for k, v := range env {
			t.Setenv(k, v)
		}
		a, _, errb := testApp(env)
		scriptedAuth(t, a, "", coreURL, "obx_key_"+strings.Repeat("f", 48))
		if code := a.run([]string{"auth"}); code != exitOK {
			t.Fatalf("auth exit = %d; stderr=%q", code, errb.String())
		}
		raw, err := os.ReadFile(filepath.Join(home, "dev.json"))
		if err != nil {
			t.Fatalf("read dev config: %v", err)
		}
		return string(raw)
	}

	cfg := run(t, nil, "http://localhost:8086")
	if !strings.Contains(cfg, `"base_url": "http://localhost:8086"`) {
		t.Errorf("the answered core URL was not persisted:\n%s", cfg)
	}
	if got, _ := devconfig.ResolveCoordinates(); got != "http://localhost:8086" {
		t.Errorf("ResolveCoordinates() base = %q, want the self-hosted core", got)
	}

	// The variable prefills the prompt, and a blank answer keeps it.
	if cfg := run(t, map[string]string{"OPENBOX_BASE_URL": "http://core.internal:8086"},
		"http://core.internal:8086"); !strings.Contains(cfg, `"base_url": "http://core.internal:8086"`) {
		t.Errorf("the self-hosted core was not persisted:\n%s", cfg)
	}

	cfg = run(t, nil, "")
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
		{"SessionEnd", "sessionend.json"}} {
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

	seedCredentials(t, "codex")
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
	kv, err := devconfig.ParseEnvFile(filepath.Join(openboxHome, "codex", ".env"))
	if err != nil {
		t.Fatalf("read credential file: %v", err)
	}
	if kv[devconfig.EnvAPIKeyDirect] != "obx_test_k" || kv[devconfig.EnvAgentPrivateKey] != testSeedB64 {
		t.Errorf("init modified the credential file: %v", kv)
	}
}

func TestHelpFlagExitsZeroForEverySubcommand(t *testing.T) {
	for _, args := range [][]string{
		{"auth", "-h"},
		{"init", "-h"},
		{"doctor", "-h"},
		{"uninstall", "-h"}} {
		a, _, _ := testApp(nil)
		if got := a.run(args); got != exitOK {
			t.Errorf("openbox %v exited %d, want 0; asking for help is not an error", args, got)
		}
	}
}

// TestUnknownFlagExitsNonZero a parse error is still an error.
func TestUnknownFlagExitsNonZero(t *testing.T) {
	a, _, _ := testApp(nil)
	if got := a.run([]string{"init", "--no-such-flag"}); got == exitOK {
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

	t.Run("a bare init governs every session and names the file", func(t *testing.T) {
		got := run(t)
		if !strings.Contains(got, "EVERY SESSION") {
			t.Errorf("one install governs every session on this machine and must say so; got:\n%s", got)
		}
		if !strings.Contains(got, filepath.Join(".claude", "settings.json")) {
			t.Errorf("say WHERE the hooks were written so it can be checked; got:\n%s", got)
		}
		// The old default governed one directory and had to warn that absence of
		// events proved nothing. That caveat is now false, and repeating it would
		// tell an auditor to distrust a complete record.
		for _, gone := range []string{"THIS PROJECT ONLY", "ANY OTHER directory are not governed", "NOTHING YET"} {
			if strings.Contains(got, gone) {
				t.Errorf("the install still describes per-directory scope (%q):\n%s", gone, got)
			}
		}
	})

	t.Run("it writes no project settings file", func(t *testing.T) {
		dir := t.TempDir()
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(wd) })

		got := run(t)
		if !strings.Contains(got, "EVERY SESSION") {
			t.Errorf("the scope statement is missing; got:\n%s", got)
		}
		if _, err := os.Stat(filepath.Join(dir, ".claude", "settings.local.json")); !os.IsNotExist(err) {
			t.Errorf("init created a project settings file: %v", err)
		}
	})
}

// seedCredentials writes a complete identity store for each named tool,
// defaulting to claude-code. Identity is per tool now, so a test that runs
// `--provider codex` has to say so or it seeds a store the command never
// reads.
func seedCredentials(t *testing.T, tools ...string) {
	t.Helper()
	if len(tools) == 0 {
		tools = []string{defaultTestProvider}
	}
	if len(tools) > 1 && os.Getenv(devconfig.EnvConfigPath) != "" {
		t.Fatal("seeding two tools under an OPENBOX_CONFIG pin would put both DIDs in one dev.json and the second would win; use isolateHomeOnly")
	}
	for _, tool := range tools {
		seedToolCredentials(t, tool, testDIDFor(t, tool))
	}
}

// testDIDFor gives each tool its own DID, so a test asserting on a spooled
// event can tell which store answered.
func testDIDFor(t *testing.T, tool string) string {
	t.Helper()
	switch tool {
	case "claude-code":
		return "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	case "codex":
		return "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c9999"
	default:
		t.Fatalf("no test DID for provider %q", tool)
		return ""
	}
}

func seedToolCredentials(t *testing.T, tool, did string) {
	t.Helper()
	envPath, err := devconfig.EnvFilePathFor(tool)
	if err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteEnvFile(envPath, map[string]string{
		devconfig.EnvAPIKeyDirect:    "obx_test_k",
		devconfig.EnvAgentPrivateKey: testSeedB64}); err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteConfig(seedDevConfigPath(t, tool), devconfig.Update{DID: did}); err != nil {
		t.Fatal(err)
	}
}

// seedDevConfigPath resolves dev.json exactly the way the command under test
// will: through the OPENBOX_CONFIG pin when the fixture set one, else the
// tool's own file. Resolving it any other way would seed a file the command
// does not read, which is the quiet way a fixture stops proving anything.
func seedDevConfigPath(t *testing.T, tool string) string {
	t.Helper()
	if p := os.Getenv(devconfig.EnvConfigPath); p != "" {
		return p
	}
	p, err := devconfig.DevConfigWritePathFor(tool)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// scriptedAuth drives `openbox auth` through its prompts, in the order they
// are asked: backend URL, core URL, organization control token. Three, not
// six: the agent id, DID, API key and signing key prompts went with the
// registration they fed, which `init --provider <tool>` owns now. A blank
// answer keeps whatever was prefilled, which is how a re-run that changes one
// URL is safe.
func scriptedAuth(t *testing.T, a *app, answers ...string) *prompt.Scripted {
	t.Helper()
	p := &prompt.Scripted{Answers: answers}
	a.newPrompt = func() (prompt.Prompter, error) { return p, nil }
	return p
}

// TestDoctorReportsControlPlaneReachability. A separate verify command proved this,
// and `doctor` inherits it: same one call, reported where somebody looking for
// a problem will actually see it.
func TestDoctorReportsControlPlaneReachability(t *testing.T) {
	srv := coreValidateOK(t, verifyTestSeed)
	setVerifyCreds(t, srv.URL)
	t.Setenv(envManagedSettingsPath, filepath.Join(t.TempDir(), "absent.json"))

	out, code := runDoctorIn(t, t.TempDir())
	if code != exitOK {
		t.Fatalf("doctor exit = %d:\n%s", code, out)
	}
	if !strings.Contains(out, "authenticated as") || !strings.Contains(out, verifyTestDID) {
		t.Errorf("doctor does not report a verified identity:\n%s", out)
	}
	if strings.Contains(out, verifyTestSeed) || strings.Contains(out, "obx_test_") {
		t.Errorf("a credential value reached doctor's output:\n%s", out)
	}
}

// TestDoctorDegradesWhenTheControlPlaneIsUnreachable is the design point, not a
// detail. doctor is what you run WHEN things are broken, so an unreachable
// control plane has to be a reported line and never a non-zero exit that
// suppresses everything below it.
func TestDoctorDegradesWhenTheControlPlaneIsUnreachable(t *testing.T) {
	// A port nothing can be listening on.
	setVerifyCreds(t, "http://127.0.0.1:1")
	t.Setenv(envManagedSettingsPath, filepath.Join(t.TempDir(), "absent.json"))

	out, code := runDoctorIn(t, t.TempDir())
	if code != exitOK {
		t.Fatalf("doctor exited %d with the control plane down; it must report and continue:\n%s", code, out)
	}
	if !strings.Contains(out, "reachable    NO") {
		t.Errorf("doctor does not report the failure:\n%s", out)
	}
	// And the rest of the report still ran.
	if !strings.Contains(out, "Hook registration") {
		t.Errorf("the unreachable control plane suppressed the rest of the report:\n%s", out)
	}
}

// TestDoctorSaysWhenThereAreNoCredentialsToCheckWith. Distinct from
// unreachable: hooks that fire, fail to resolve credentials and fail open
// govern nothing while looking installed, so the remedy has to be named.
func TestDoctorSaysWhenThereAreNoCredentialsToCheckWith(t *testing.T) {
	isolateHome(t)
	t.Setenv("OPENBOX_API_KEY", "")
	t.Setenv("OPENBOX_AGENT_PRIVATE_KEY", "")
	t.Setenv("OPENBOX_ED25519_SEED", "")
	t.Setenv(envManagedSettingsPath, filepath.Join(t.TempDir(), "absent.json"))

	out, code := runDoctorIn(t, t.TempDir())
	if code != exitOK {
		t.Fatalf("doctor exit = %d:\n%s", code, out)
	}
	// Per tool, and naming the command that fixes it. `auth` connects the
	// organization and writes no agent credential, so pointing a reader there
	// would send them somewhere that cannot resolve this.
	if !strings.Contains(out, "NOT CHECKED") {
		t.Errorf("doctor does not say the check was skipped:\n%s", out)
	}
	for _, tool := range provider.Supported() {
		if !strings.Contains(out, "openbox init --provider "+tool) {
			t.Errorf("doctor does not name the remedy for %s:\n%s", tool, out)
		}
	}
}
