package devinit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/backend"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

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
	installErr error
	installed  bool
	gotRef     provider.CredentialRef
}

func (f *fakeInstaller) Name() provider.Name { return provider.ClaudeCode }
func (f *fakeInstaller) Install(r provider.CredentialRef) error {
	f.gotRef = r
	if f.installErr != nil {
		return f.installErr
	}
	f.installed = true
	return nil
}

func isolateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(devconfig.EnvHome, dir)
	t.Setenv(devconfig.EnvConfigPath, filepath.Join(dir, "dev.json"))
	t.Setenv(devconfig.EnvDID, "")
	bindProviderForTest(t, "claude-code")
	return dir
}

// bindProviderForTest binds the tool whose identity store this package writes.
// In production `openbox init --provider X` binds before devinit runs, so a
// fixture that left this unbound would exercise a path no command takes.
func bindProviderForTest(t *testing.T, tool string) {
	t.Helper()
	release, err := devconfig.BindProvider(tool)
	if err != nil {
		t.Fatalf("bind %s: %v", tool, err)
	}
	t.Cleanup(release)
}

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

func TestHappyPathStoresCredsNeverPrintsThem(t *testing.T) {
	isolateHome(t)
	reg := &fakeRegistrar{reg: validReg()}
	inst := &fakeInstaller{}
	var out bytes.Buffer

	res, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{Registrar: reg, Installer: inst, Out: &out})

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Registered || res.AgentID != "agent-1" {
		t.Errorf("res = %+v", res)
	}
	kv := readCredentialFile(t)
	if got := kv[devconfig.EnvAPIKeyDirect]; got != "obx_test_SECRETKEYVALUE" {
		t.Errorf("api key not written, got %q", got)
	}
	if got := kv[devconfig.EnvAgentPrivateKey]; got != "PRIVATESEEDVALUE" {
		t.Errorf("private key not written, got %q", got)
	}
	if got, ok := kv[devconfig.EnvDID]; ok {
		t.Errorf("credential file carries the DID (%q); secrets and coordinates must not share a file", got)
	}
	if len(kv) != 2 {
		t.Errorf("credential file holds %d keys, want exactly the 2 secrets: %v", len(kv), kv)
	}
	if strings.Contains(out.String(), "obx_test_SECRETKEYVALUE") || strings.Contains(out.String(), "PRIVATESEEDVALUE") {
		t.Errorf("secret leaked to output:\n%s", out.String())
	}
	if reg.lastReq.AgentType != "developer" || reg.lastReq.Icon == "" {
		t.Errorf("bad create request: %+v", reg.lastReq)
	}
}

func TestConfigAppliedWhenInstallerAvailable(t *testing.T) {
	isolateHome(t)
	reg := &fakeRegistrar{reg: validReg()}
	inst := &fakeInstaller{}
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
	// The tool's own store, not the org one: reuse is a strictly per-tool
	// decision, so seeding ~/.openbox/.env here would seed a file this run
	// never reads.
	envPath, err := devconfig.EnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "claude-code", ".env"); envPath != want {
		t.Fatalf("credential file = %q, want the per-tool store %q", envPath, want)
	}
	if err := devconfig.WriteEnvFile(envPath, map[string]string{
		devconfig.EnvAPIKeyDirect:    "obx_test_existing",
		devconfig.EnvAgentPrivateKey: "existingseed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteConfig(filepath.Join(dir, "dev.json"), devconfig.Update{DID: "did:aip:existing"}); err != nil {
		t.Fatal(err)
	}

	reg := &fakeRegistrar{reg: validReg()}
	inst := &fakeInstaller{}
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
		Deps{Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
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
	// It must surface and stop.
	reg := &fakeRegistrar{reg: validReg(), findErr: errors.New("connection refused")}
	res, err := Run(context.Background(), Options{Provider: "claude-code", AgentName: "dev-x"},
		Deps{Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
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

func TestAPIErrorHalts(t *testing.T) {
	isolateHome(t)
	reg := &fakeRegistrar{createErr: &backend.APIError{StatusCode: 400, Body: "AIVSS config is required"}}
	res, err := Run(context.Background(), Options{Provider: "claude-code", AgentName: "dev-x"},
		Deps{Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "HALT") || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected HALT with 400, got %v", err)
	}
	if res.Registered {
		t.Error("should not be marked registered on API error")
	}
}

// TestPartialFailureReportsAgentAndResume a credential write that fails after
// the agent exists must name the registered agent and how to resume: its API
// key and signing key were shown exactly once and are now unreachable, so a
// bare I/O error would strand the user.
func TestPartialFailureReportsAgentAndResume(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not deny writes on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny writes")
	}
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
		Deps{Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "agent-1") || !strings.Contains(err.Error(), "rotate") {
		t.Fatalf("expected resume guidance naming agent-1, got %v", err)
	}
	if !res.Registered {
		t.Error("agent was registered; res.Registered should be true")
	}
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
		Deps{Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "signing key") {
		t.Fatalf("expected signing-key error, got %v", err)
	}
	if kv := readCredentialFile(t); len(kv) != 0 {
		t.Errorf("no credentials should be written when the key is missing, got %v", kv)
	}
}

func TestTruncateIsRuneSafe(t *testing.T) {
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

// TestDefaultAgentNameIsToolScoped a machine holds one agent per governed
// tool, and two agents cannot share a name in an org -- so a name that omitted
// the tool would collide with itself on the second `init`, and the collision
// surfaces as a halt naming an agent the developer has never heard of.
func TestDefaultAgentNameIsToolScoped(t *testing.T) {
	cc := defaultAgentName("claude-code")
	cx := defaultAgentName("codex")
	if !strings.HasPrefix(cc, "claude-code-") {
		t.Errorf("claude-code name = %q, want it to start with the tool", cc)
	}
	if !strings.HasPrefix(cx, "codex-") {
		t.Errorf("codex name = %q, want it to start with the tool", cx)
	}
	if cc == cx {
		t.Fatal("two tools derived the same default agent name")
	}
	for _, name := range []string{cc, cx} {
		if !strings.Contains(name, "@") {
			t.Errorf("name %q carries no host part", name)
		}
		if strings.HasPrefix(name, "openbox-dev-") {
			t.Errorf("name %q still uses the machine-wide shape", name)
		}
	}
}

// TestNilRegistrarIsANamedError the caller wires a registrar on the register
// branch only, so a nil one here is a wiring defect. It used to be a nil
// dereference, which surfaced as a stack trace from a path where every other
// failure is a sentence.
func TestNilRegistrarIsANamedError(t *testing.T) {
	isolateHome(t)
	res, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
	if err == nil {
		t.Fatalf("Run with no registrar and no store returned no error (res=%+v)", res)
	}
	if !strings.Contains(err.Error(), "registrar") {
		t.Errorf("error = %v, want it to name the missing registrar", err)
	}
}

// TestReuseNeverFiresFromTheOrgStore the no-fallback rule, stated as the thing
// that could silently undo it. A complete agent identity at the org level is
// exactly what an upgraded machine has, and reading it here would make one
// tool's install decide it is already registered because a previous, tool-less
// install was -- then install hooks that sign as that agent.
func TestReuseNeverFiresFromTheOrgStore(t *testing.T) {
	dir := isolateHome(t)
	if err := devconfig.WriteEnvFile(filepath.Join(dir, ".env"), map[string]string{
		devconfig.EnvAPIKeyDirect:    "obx_org_level_key",
		devconfig.EnvAgentPrivateKey: "orgseed",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "claude-code")); !os.IsNotExist(err) {
		t.Fatalf("the fixture already has a per-tool store; this case would prove nothing (err=%v)", err)
	}

	reg := &fakeRegistrar{reg: validReg()}
	res, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Reused {
		t.Fatal("the reuse branch fired from the org-level store")
	}
	if !res.Registered || reg.createCalls != 1 {
		t.Errorf("want a fresh registration, got %+v (creates=%d)", res, reg.createCalls)
	}
}

// TestTheDuplicateNameHaltNamesTheCauseAndTheRemedy (owner ruling V4.) This
// halt is what an upgraded machine hits when its old agent still exists in the
// org under a name this install would reuse, and the remedy it used to give --
// re-run `auth` and paste the agent id -- is a route that no longer exists.
// A halt that names a dead route is worse than one that names none.
func TestTheDuplicateNameHaltNamesTheCauseAndTheRemedy(t *testing.T) {
	isolateHome(t)
	name := defaultAgentName("claude-code")
	reg := &fakeRegistrar{
		reg:    validReg(),
		byName: map[string]*backend.AgentSummary{name: {ID: "old-9", DID: "did:aip:old"}},
	}
	_, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("a name already taken in the org must halt, not mint a second agent")
	}
	msg := err.Error()
	for _, want := range []string{name, "old-9", "did:aip:old", "openbox init --provider claude-code", "adopt"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the halt does not name %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "openbox auth") {
		t.Errorf("the halt still offers `openbox auth`, which no longer registers anything:\n%s", msg)
	}
	if reg.createCalls != 0 {
		t.Errorf("Create was called %d times after the halt", reg.createCalls)
	}
}

// TestARejectedCredentialIsNotReportedAsAnOutage a 401 means the backend
// answered, so telling the operator to "re-run when the OpenBox org is
// reachable" is advice that can never work: they will wait for connectivity
// they already have. What actually produced it is a credential that does not
// belong to the backend being contacted -- most often an organization token
// for one deployment sent to another, because `auth` accepted the hosted URL
// defaults for a self-hosted org.
//
// Measured on a real machine: the same production client and the same token
// return 200 against the org's own backend and this exact 401 against the
// hosted default. Neither the key nor the network was at fault, and the
// message named neither the real cause nor the host it had contacted.
func TestARejectedCredentialIsNotReportedAsAnOutage(t *testing.T) {
	for _, status := range []int{401, 403} {
		isolateHome(t)
		reg := &fakeRegistrar{findErr: &backend.APIError{
			StatusCode: status,
			Body:       `{"status":` + strconv.Itoa(status) + `,"message":"Invalid API key"}`,
			URL:        "https://api.openbox.ai/agent/list?all=true",
		}}
		_, err := Run(context.Background(), Options{Provider: "claude-code"},
			Deps{Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
		if err == nil {
			t.Fatalf("HTTP %d did not fail the run", status)
		}
		msg := err.Error()

		if strings.Contains(msg, "reachable") {
			t.Errorf("HTTP %d reported as an outage; the backend answered:\n%s", status, msg)
		}
		// The one fact that makes this self-diagnosable, and the one the old
		// message omitted: which backend was actually contacted.
		if !strings.Contains(msg, "https://api.openbox.ai") {
			t.Errorf("HTTP %d does not name the backend it contacted:\n%s", status, msg)
		}
		for _, want := range []string{"openbox auth", "OPENBOX_CONTROL_TOKEN"} {
			if !strings.Contains(msg, want) {
				t.Errorf("HTTP %d does not name %q as the place to fix it:\n%s", status, want, msg)
			}
		}
		if reg.createCalls != 0 {
			t.Errorf("a rejected credential still reached agent/create")
		}
	}
}

// TestAnUnreachableBackendStillReadsAsAnOutage the other half. Removing the
// outage wording for a 401 must not remove it for the case it was written
// for, or the next person to lose their network is told to check a key.
func TestAnUnreachableBackendStillReadsAsAnOutage(t *testing.T) {
	isolateHome(t)
	reg := &fakeRegistrar{findErr: errors.New("dial tcp 10.0.0.1:443: connect: connection refused")}
	_, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("a dial failure did not fail the run")
	}
	if !strings.Contains(err.Error(), "reachable") {
		t.Errorf("a genuine outage lost its wording:\n%s", err)
	}
}

// TestABackendErrorNamesTheHostItCameFrom every non-2xx carries the URL that
// produced it, so no caller has to reconstruct which deployment answered.
// Minting an agent against one backend while posting its events to another is
// the failure this makes visible, and it is otherwise invisible until a 401
// arrives much later with nothing naming a URL.
func TestABackendErrorNamesTheHostItCameFrom(t *testing.T) {
	e := &backend.APIError{StatusCode: 401, Body: `{"message":"Invalid API key"}`,
		URL: "https://api.openbox.ai/agent/list?all=true"}
	msg := e.Error()
	if !strings.Contains(msg, "https://api.openbox.ai/agent/list?all=true") {
		t.Errorf("APIError does not name its URL: %s", msg)
	}
	if !strings.Contains(msg, "401") || !strings.Contains(msg, "Invalid API key") {
		t.Errorf("APIError lost its status or body: %s", msg)
	}
	// An error built without a URL must still read cleanly.
	if got := (&backend.APIError{StatusCode: 500, Body: "boom"}).Error(); strings.Contains(got, "()") {
		t.Errorf("a URL-less APIError reads badly: %s", got)
	}
}
