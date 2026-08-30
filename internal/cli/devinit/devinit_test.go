package devinit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/backend"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// --- fakes ------------------------------------------------------------------

type fakeRegistrar struct {
	createCalls, findCalls int
	reg                    *backend.Registration
	createErr              error
	findErr                error
	byName                 map[string]*backend.AgentSummary
	lastReq                backend.CreateAgentRequest
}

func (f *fakeRegistrar) Create(_ context.Context, req backend.CreateAgentRequest) (*backend.Registration, error) {
	f.createCalls++
	f.lastReq = req
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.reg, nil
}

func (f *fakeRegistrar) FindByName(_ context.Context, name string) (*backend.AgentSummary, error) {
	f.findCalls++
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.byName[name], nil
}

type fakeInstaller struct {
	avail      bool
	installErr error
	installed  bool
	gotRef     provider.CredentialRef
}

func (f *fakeInstaller) Name() provider.Name                  { return provider.ClaudeCode }
func (f *fakeInstaller) Available() bool                      { return f.avail }
func (f *fakeInstaller) Plan(r provider.CredentialRef) string { return "MANUAL-CONFIG did=" + r.DID }
func (f *fakeInstaller) Install(r provider.CredentialRef) error {
	f.gotRef = r
	if f.installErr != nil {
		return f.installErr
	}
	f.installed = true
	return nil
}

// isolateHome points OPENBOX_HOME at a temp dir so a test writes its credential
// file there instead of the developer's real ~/.openbox.
//
// This replaced an injected secret.Store. With credentials in a plaintext file
// the test can exercise the production write path rather than a stand-in for
// it, which is why there is no MemStore any more.
func isolateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(devconfig.EnvHome, dir)
	t.Setenv(devconfig.EnvConfigPath, filepath.Join(dir, "dev.json"))
	t.Setenv(devconfig.EnvDID, "")
	return dir
}

// readCredentialFile reads what the run wrote, for assertions.
func readCredentialFile(t *testing.T) map[string]string {
	t.Helper()
	path, err := devconfig.EnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	kv, err := devconfig.ParseEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return kv
}

func validReg() *backend.Registration {
	return &backend.Registration{
		AgentID:    "agent-1",
		AgentName:  "dev-x",
		DID:        "did:aip:abc",
		APIKey:     "obx_test_SECRETKEYVALUE",
		PrivateKey: "PRIVATESEEDVALUE",
		Tier:       "Tier 2",
		TrustScore: "0.81",
	}
}

// --- tests ------------------------------------------------------------------

func TestDryRunMakesNoWrites(t *testing.T) {
	home := isolateHome(t)
	reg := &fakeRegistrar{}
	inst := &fakeInstaller{avail: false}
	var out bytes.Buffer

	_, err := Run(context.Background(), Options{Provider: "claude-code", DryRun: true},
		Deps{Registrar: reg, Installer: inst, Out: &out})
	if err != nil {
		t.Fatalf("dry-run err: %v", err)
	}
	if reg.createCalls != 0 || reg.findCalls != 0 {
		t.Errorf("dry-run touched the network: create=%d find=%d", reg.createCalls, reg.findCalls)
	}
	if entries, err := os.ReadDir(home); err == nil && len(entries) != 0 {
		t.Errorf("dry-run wrote %v; it must touch no file at all", entries)
	}
	if !strings.Contains(out.String(), "DRY RUN") {
		t.Errorf("dry-run output missing DRY RUN banner:\n%s", out.String())
	}
}

func TestDryRunDisclosesInstallGitHook(t *testing.T) {
	isolateHome(t)
	var out bytes.Buffer
	_, err := Run(context.Background(),
		Options{Provider: "claude-code", DryRun: true, InstallGitHook: true},
		Deps{Registrar: &fakeRegistrar{}, Installer: &fakeInstaller{avail: false}, Out: &out})
	if err != nil {
		t.Fatalf("dry-run err: %v", err)
	}
	if !strings.Contains(out.String(), "install_git_hook: true") {
		t.Errorf("dry-run must disclose the ambient commit-hook install:\n%s", out.String())
	}
}

func TestHappyPathStoresCredsNeverPrintsThem(t *testing.T) {
	isolateHome(t)
	reg := &fakeRegistrar{reg: validReg()}
	inst := &fakeInstaller{avail: false} // Claude Code adapter (SL-4) not built
	var out bytes.Buffer

	res, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{Registrar: reg, Installer: inst, Out: &out})

	// Registration + credential capture succeed; config is manual-only → error.
	if err == nil || !res.ConfigManualOnly {
		t.Fatalf("expected manual-config error, got err=%v res=%+v", err, res)
	}
	if !res.Registered || res.AgentID != "agent-1" {
		t.Errorf("res = %+v", res)
	}
	// Exactly the two secrets, under the platform's documented names.
	kv := readCredentialFile(t)
	if got := kv[devconfig.EnvAPIKeyDirect]; got != "obx_test_SECRETKEYVALUE" {
		t.Errorf("api key not written, got %q", got)
	}
	if got := kv[devconfig.EnvAgentPrivateKey]; got != "PRIVATESEEDVALUE" {
		t.Errorf("private key not written, got %q", got)
	}
	// The DID is a COORDINATE and must not be in the credential file. Writing it
	// there would recreate the two-store bug that decision removed: a stale copy
	// beside the secrets that reverts a corrected DID on the next install.
	if got, ok := kv[devconfig.EnvDID]; ok {
		t.Errorf("credential file carries the DID (%q); secrets and coordinates must not share a file", got)
	}
	if len(kv) != 2 {
		t.Errorf("credential file holds %d keys, want exactly the 2 secrets: %v", len(kv), kv)
	}
	// INV-1: secret values must never appear in output.
	if strings.Contains(out.String(), "obx_test_SECRETKEYVALUE") || strings.Contains(out.String(), "PRIVATESEEDVALUE") {
		t.Errorf("secret leaked to output:\n%s", out.String())
	}
	// Manual config surfaced.
	if !strings.Contains(out.String(), "MANUAL-CONFIG") {
		t.Errorf("manual config not printed:\n%s", out.String())
	}
	// Correct contract sent.
	if reg.lastReq.AgentType != "developer" || reg.lastReq.Icon == "" {
		t.Errorf("bad create request: %+v", reg.lastReq)
	}
}

func TestConfigAppliedWhenInstallerAvailable(t *testing.T) {
	isolateHome(t)
	reg := &fakeRegistrar{reg: validReg()}
	inst := &fakeInstaller{avail: true}
	res, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{Registrar: reg, Installer: inst, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.ConfigApplied || !inst.installed {
		t.Errorf("config not applied: res=%+v installed=%v", res, inst.installed)
	}
	if inst.gotRef.DID != "did:aip:abc" {
		t.Errorf("installer got bad ref: %+v", inst.gotRef)
	}
}

func TestIdempotentReuseSkipsRegistration(t *testing.T) {
	dir := isolateHome(t)
	if err := devconfig.WriteEnvFile(filepath.Join(dir, ".env"), map[string]string{
		devconfig.EnvAPIKeyDirect:    "obx_test_existing",
		devconfig.EnvAgentPrivateKey: "existingseed",
	}); err != nil {
		t.Fatal(err)
	}
	// The DID is a coordinate and lives in dev.json, never beside the secrets —
	// that split is what stopped a stale credential store from reverting a
	// corrected DID on every re-init.
	if err := devconfig.WriteConfig(filepath.Join(dir, "dev.json"), devconfig.Update{DID: "did:aip:existing"}); err != nil {
		t.Fatal(err)
	}

	reg := &fakeRegistrar{reg: validReg()}
	inst := &fakeInstaller{avail: true}
	res, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{Registrar: reg, Installer: inst, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("reuse err: %v", err)
	}
	if reg.createCalls != 0 || reg.findCalls != 0 {
		t.Errorf("reuse should not hit the network: create=%d find=%d", reg.createCalls, reg.findCalls)
	}
	if !res.Reused || res.DID != "did:aip:existing" {
		t.Errorf("res = %+v", res)
	}
}

func TestRemoteDuplicateBlocksWithoutForce(t *testing.T) {
	isolateHome(t)
	reg := &fakeRegistrar{
		reg:    validReg(),
		byName: map[string]*backend.AgentSummary{"dev-x": {ID: "old-9", DID: "did:aip:old"}},
	}
	res, err := Run(context.Background(), Options{Provider: "claude-code", AgentName: "dev-x"},
		Deps{Registrar: reg, Installer: &fakeInstaller{avail: true}, Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
	if reg.createCalls != 0 {
		t.Errorf("must not create over an existing agent: create=%d", reg.createCalls)
	}
	if res.AgentID != "old-9" {
		t.Errorf("res.AgentID = %q, want old-9", res.AgentID)
	}
}

func TestRemoteLookupErrorDoesNotFallThroughToCreate(t *testing.T) {
	isolateHome(t)
	// F1: a failed agent/list must NOT silently proceed to Create (which would
	// bypass idempotent detection). It must surface and stop.
	reg := &fakeRegistrar{reg: validReg(), findErr: errors.New("connection refused")}
	res, err := Run(context.Background(), Options{Provider: "claude-code", AgentName: "dev-x"},
		Deps{Registrar: reg, Installer: &fakeInstaller{avail: true}, Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "agent/list failed") {
		t.Fatalf("expected surfaced list error, got %v", err)
	}
	if reg.createCalls != 0 {
		t.Errorf("must not create when the duplicate check failed: create=%d", reg.createCalls)
	}
	if res.Registered {
		t.Error("should not be registered")
	}
}

func TestForceLookupErrorSurfaced(t *testing.T) {
	isolateHome(t)
	// F4: under --force, a failed lookup must surface rather than proceed to a
	// confusing 400 with a name that may be taken.
	reg := &fakeRegistrar{reg: validReg(), findErr: errors.New("connection refused")}
	_, err := Run(context.Background(), Options{Provider: "claude-code", AgentName: "dev-x", Force: true},
		Deps{Registrar: reg, Installer: &fakeInstaller{avail: true}, Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "free agent name") {
		t.Fatalf("expected free-name lookup error, got %v", err)
	}
	if reg.createCalls != 0 {
		t.Errorf("must not create when the free-name lookup failed: create=%d", reg.createCalls)
	}
}

func TestForceRegistersUnderFreeName(t *testing.T) {
	isolateHome(t)
	reg := &fakeRegistrar{
		reg:    validReg(),
		byName: map[string]*backend.AgentSummary{"dev-x": {ID: "old-9"}}, // dev-x-2 is free
	}
	inst := &fakeInstaller{avail: true}
	_, err := Run(context.Background(), Options{Provider: "claude-code", AgentName: "dev-x", Force: true},
		Deps{Registrar: reg, Installer: inst, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("force err: %v", err)
	}
	if reg.lastReq.AgentName != "dev-x-2" {
		t.Errorf("forced name = %q, want dev-x-2", reg.lastReq.AgentName)
	}
	if reg.createCalls != 1 {
		t.Errorf("create calls = %d, want 1", reg.createCalls)
	}
}

func TestAPIErrorHalts(t *testing.T) {
	isolateHome(t)
	reg := &fakeRegistrar{createErr: &backend.APIError{StatusCode: 400, Body: "AIVSS config is required"}}
	res, err := Run(context.Background(), Options{Provider: "claude-code", AgentName: "dev-x"},
		Deps{Registrar: reg, Installer: &fakeInstaller{avail: true}, Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "HALT") || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected HALT with 400, got %v", err)
	}
	if res.Registered {
		t.Error("should not be marked registered on API error")
	}
}

// A credential write that fails AFTER the agent exists must name the registered
// agent and how to resume: its API key and signing key were shown exactly once
// and are now unreachable, so a bare I/O error would strand the user.
func TestPartialFailureReportsAgentAndResume(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not deny writes on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny writes")
	}
	// An unwritable parent makes the REAL write fail, where the deleted fake
	// store could only report that it would have.
	base := t.TempDir()
	locked := filepath.Join(base, "locked")
	if err := os.MkdirAll(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	t.Setenv(devconfig.EnvHome, filepath.Join(locked, "openbox"))
	t.Setenv(devconfig.EnvConfigPath, filepath.Join(base, "dev.json"))

	reg := &fakeRegistrar{reg: validReg()}
	res, err := Run(context.Background(), Options{Provider: "claude-code", AgentName: "dev-x"},
		Deps{Registrar: reg, Installer: &fakeInstaller{avail: true}, Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "agent-1") || !strings.Contains(err.Error(), "rotate") {
		t.Fatalf("expected resume guidance naming agent-1, got %v", err)
	}
	if !res.Registered {
		t.Error("agent was registered; res.Registered should be true")
	}
	// Nothing printed may leak the credential itself.
	if strings.Contains(err.Error(), "obx_test_SECRETKEYVALUE") || strings.Contains(err.Error(), "PRIVATESEEDVALUE") {
		t.Errorf("error leaked a credential value: %v", err)
	}
}

func TestMissingPrivateKeyErrors(t *testing.T) {
	isolateHome(t)
	r := validReg()
	r.PrivateKey = ""
	reg := &fakeRegistrar{reg: r}
	_, err := Run(context.Background(), Options{Provider: "claude-code", AgentName: "dev-x"},
		Deps{Registrar: reg, Installer: &fakeInstaller{avail: true}, Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "signing key") {
		t.Fatalf("expected signing-key error, got %v", err)
	}
	// A half-usable credential pair must never reach disk.
	if kv := readCredentialFile(t); len(kv) != 0 {
		t.Errorf("no credentials should be written when the key is missing, got %v", kv)
	}
}

func TestTruncateIsRuneSafe(t *testing.T) {
	// F5: truncation must not split a multibyte rune (invalid UTF-8 would 400).
	s := strings.Repeat("a", 254) + "é" // 'é' is 2 bytes -> byte 255 is a continuation
	got := truncate(s, 255)
	if len(got) > 255 {
		t.Fatalf("truncate exceeded 255 bytes: %d", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncate produced invalid UTF-8")
	}
	if truncate("short", 255) != "short" {
		t.Error("truncate altered a short string")
	}
}

func TestUnknownProviderRejected(t *testing.T) {
	isolateHome(t)
	_, err := Run(context.Background(), Options{Provider: ""},
		Deps{Registrar: &fakeRegistrar{}, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("expected error for empty provider")
	}
}

// `--env-file` must reach the REGISTER path, which is the one place credentials are
// minted. It used to be threaded into the direct-write and rotate paths only, so
// `openbox auth --env-file /custom` with a blank agent id created a real agent and
// wrote its once-shown key to the DEFAULT location while reporting the custom one.
// A once-shown credential written somewhere the user was not told about is
// unrecoverable by definition.
func TestRegisterHonoursTheCredentialFileOverride(t *testing.T) {
	isolateHome(t)
	custom := filepath.Join(t.TempDir(), "custom-creds.env")
	reg := &fakeRegistrar{reg: validReg()}

	_, _, err := Register(context.Background(), Options{
		Provider: "claude-code", EnvFile: custom,
	}, Deps{Registrar: reg, Installer: &fakeInstaller{avail: true}, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	kv, readErr := devconfig.ParseEnvFile(custom)
	if readErr != nil {
		t.Fatalf("read the override path: %v", readErr)
	}
	if kv[devconfig.EnvAPIKeyDirect] != "obx_test_SECRETKEYVALUE" {
		t.Errorf("credentials did not land in the override path: %v", kv)
	}
	// And nothing went to the default location.
	if def := readCredentialFile(t); len(def) != 0 {
		t.Errorf("credentials also written to the default path: %v", def)
	}
}

// A relative override would resolve against the process working directory, so
// running this inside a repo would drop a plaintext API key and signing key into
// the source tree. Refused, like OPENBOX_HOME.
func TestRegisterRefusesARelativeCredentialFileOverride(t *testing.T) {
	isolateHome(t)
	reg := &fakeRegistrar{reg: validReg()}
	_, _, err := Register(context.Background(), Options{
		Provider: "claude-code", EnvFile: "creds.env",
	}, Deps{Registrar: reg, Installer: &fakeInstaller{avail: true}, Out: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("a relative --env-file was accepted")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error should explain the absolute-path requirement: %v", err)
	}
}
