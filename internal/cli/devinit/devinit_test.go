package devinit

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
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
	"github.com/openbox-ai/openbox-shift-left/internal/client/workloadauth"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// fixedTestKey is generated once per test binary (never a literal, per the
// repo's standing rule) and shared by every register() call in this file via
// fixedGenerateKey, so a test asserting on the written workload key does not
// pay for a fresh 2048-bit keygen per case.
var fixedTestKey, fixedTestKeyErr = rsa.GenerateKey(rand.Reader, 2048)

func fixedGenerateKey() (*rsa.PrivateKey, error) { return fixedTestKey, fixedTestKeyErr }

type fakeRegistrar struct {
	createCalls, findCalls int
	reg                    *backend.Registration
	createErr              error
	findErr                error
	byName                 map[string]*backend.AgentSummary
	lastReq                backend.CreateAgentRequest
	// kidMismatch forces the returned Identity.Kid to stay whatever f.reg
	// already set, instead of echoing the submitted JWK's own kid -- the one
	// case that must NOT match, so the guard has something to catch.
	kidMismatch bool
}

func (f *fakeRegistrar) Create(_ context.Context, req backend.CreateAgentRequest) (*backend.Registration, error) {
	f.createCalls++
	f.lastReq = req
	if f.createErr != nil {
		return nil, f.createErr
	}
	reg := *f.reg
	// A real backend persists exactly the kid this machine submitted
	// (agent-registration-identity.service.ts:429); echoing it here means a
	// fixture's own Identity.Kid literal never has to match what
	// workloadauth.PublicJWK derives from whichever key the case generated.
	if !f.kidMismatch && req.IdentityVerification != nil {
		reg.Identity.Kid = req.IdentityVerification.PublicJWK["kid"]
	}
	return &reg, nil
}

func (f *fakeRegistrar) FindByName(_ context.Context, name string) (*backend.AgentSummary, error) {
	f.findCalls++
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.byName[name], nil
}

// raceRegistrar simulates Create's own duplicate-name race: FindByName sees
// nothing (or whatever byName says), but Create itself answers HTTP 400
// "already exists in this organization" for a specific name (duplicateFor) or
// for every call (alwaysDuplicate), the shape backend.IsDuplicateNameConflict
// classifies.
type raceRegistrar struct {
	createCalls int
	reg         *backend.Registration
	byName      map[string]*backend.AgentSummary
	lastReq     backend.CreateAgentRequest

	duplicateFor    string
	alwaysDuplicate bool
}

func (r *raceRegistrar) FindByName(_ context.Context, name string) (*backend.AgentSummary, error) {
	return r.byName[name], nil
}

func (r *raceRegistrar) Create(_ context.Context, req backend.CreateAgentRequest) (*backend.Registration, error) {
	r.createCalls++
	r.lastReq = req
	if r.alwaysDuplicate || req.AgentName == r.duplicateFor {
		return nil, &backend.APIError{StatusCode: 400,
			Body: `{"message":"Agent with name \"` + req.AgentName + `\" already exists in this organization"}`}
	}
	reg := *r.reg
	if req.IdentityVerification != nil {
		reg.Identity.Kid = req.IdentityVerification.PublicJWK["kid"]
	}
	return &reg, nil
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
	t.Setenv(devconfig.EnvAgentID, "")
	t.Setenv(devconfig.EnvAPIKeyDirect, "")
	t.Setenv(devconfig.EnvWorkloadPrivateKey, "")
	t.Setenv(devconfig.EnvAgentPrivateKey, "")
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
		APIKey:     "obx_test_SECRETKEYVALUE",
		Tier:       "Tier 2",
		TrustScore: "0.81",
		Identity: backend.WorkloadIdentityInfo{
			Method:     devconfig.IdentityMethodKeycloakWorkload,
			SourceType: "openbox",
			Kid:        "kid-xyz",
		},
	}
}

func TestHappyPathStoresCredsNeverPrintsThem(t *testing.T) {
	isolateHome(t)
	reg := &fakeRegistrar{reg: validReg()}
	inst := &fakeInstaller{}
	var out bytes.Buffer

	res, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: inst, Out: &out})

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
	wantWorkloadKey, err := workloadauth.EncodePrivateKey(fixedTestKey)
	if err != nil {
		t.Fatal(err)
	}
	if got := kv[devconfig.EnvWorkloadPrivateKey]; got != wantWorkloadKey {
		t.Errorf("workload key not written, got %q want %q", got, wantWorkloadKey)
	}
	if got, ok := kv[devconfig.EnvDID]; ok {
		t.Errorf("credential file carries the DID (%q); secrets and coordinates must not share a file", got)
	}
	if len(kv) != 2 {
		t.Errorf("credential file holds %d keys, want exactly the 2 secrets: %v", len(kv), kv)
	}
	if strings.Contains(out.String(), "obx_test_SECRETKEYVALUE") || strings.Contains(out.String(), wantWorkloadKey) {
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
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: inst, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.ConfigApplied || !inst.installed {
		t.Errorf("config not applied: res=%+v installed=%v", res, inst.installed)
	}
	if inst.gotRef.AgentID != "agent-1" {
		t.Errorf("installer got bad ref: %+v", inst.gotRef)
	}
	if inst.gotRef.IdentityMethod != devconfig.IdentityMethodKeycloakWorkload {
		t.Errorf("installer ref identity method = %q, want %q", inst.gotRef.IdentityMethod, devconfig.IdentityMethodKeycloakWorkload)
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
		devconfig.EnvAPIKeyDirect:       "obx_test_existing",
		devconfig.EnvWorkloadPrivateKey: "wk_existing",
	}); err != nil {
		t.Fatal(err)
	}
	const existingAgentID = "eeeeeeee-0000-5000-a000-0000000000ee"
	// isolateHome pins OPENBOX_CONFIG to dir/dev.json; ResolveAgentID (via
	// devconfig.load()) honours that override, same as the production hot path.
	if err := devconfig.WriteConfig(filepath.Join(dir, "dev.json"), devconfig.Update{
		AgentID: existingAgentID, IdentityMethod: devconfig.IdentityMethodKeycloakWorkload,
	}); err != nil {
		t.Fatal(err)
	}

	reg := &fakeRegistrar{reg: validReg()}
	inst := &fakeInstaller{}
	res, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: inst, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("reuse err: %v", err)
	}
	if reg.createCalls != 0 || reg.findCalls != 0 {
		t.Errorf("reuse should not hit the network: create=%d find=%d", reg.createCalls, reg.findCalls)
	}
	if !res.Reused || res.AgentID != existingAgentID || res.IdentityMethod != devconfig.IdentityMethodKeycloakWorkload {
		t.Errorf("res = %+v, want AgentID %q identity method %q", res, existingAgentID, devconfig.IdentityMethodKeycloakWorkload)
	}
}

// TestLegacyStoreDoesNotReuse a stored developer_did or a seed under the
// current or a deprecated name never satisfies the reuse gate: the v3 binary
// cannot speak for that identity at all, so it falls through to registering a
// new one, same as an empty store.
func TestLegacyStoreDoesNotReuse(t *testing.T) {
	dir := isolateHome(t)
	envPath, err := devconfig.EnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteEnvFile(envPath, map[string]string{
		devconfig.EnvAPIKeyDirect:    "obx_test_existing",
		devconfig.EnvAgentPrivateKey: "existingseed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := devconfig.SetLegacyDID(filepath.Join(dir, "dev.json"), "did:aip:existing"); err != nil {
		t.Fatal(err)
	}

	reg := &fakeRegistrar{reg: validReg()}
	inst := &fakeInstaller{}
	res, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: inst, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Reused {
		t.Fatal("a legacy store was reused; the v3 binary cannot speak for it")
	}
	if !res.Registered || reg.createCalls != 1 {
		t.Errorf("want a fresh registration over the legacy store, got %+v (creates=%d)", res, reg.createCalls)
	}
}

// TestExplicitNameTakenAlsoRegistersSuffixed the taken-name branch is
// unconditional: an operator-chosen --name that collides gets the same
// suffixed registration a default name does, rather than a halt.
func TestExplicitNameTakenAlsoRegistersSuffixed(t *testing.T) {
	isolateHome(t)
	reg := &fakeRegistrar{
		reg:    validReg(),
		byName: map[string]*backend.AgentSummary{"dev-x": {ID: "old-9", AgentName: "dev-x"}},
	}
	var out bytes.Buffer
	res, err := Run(context.Background(), Options{Provider: "claude-code", AgentName: "dev-x"},
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &out,
			Suffix: func() (string, error) { return "a1b2c3", nil }})
	if err != nil {
		t.Fatalf("a taken explicit name must register suffixed, not fail: %v", err)
	}
	if reg.createCalls != 1 {
		t.Errorf("want exactly one Create, got %d", reg.createCalls)
	}
	if reg.lastReq.AgentName != "dev-x-a1b2c3" {
		t.Errorf("Create got name %q, want the suffixed name", reg.lastReq.AgentName)
	}
	if !res.Registered {
		t.Error("res.Registered should be true")
	}
	if s := out.String(); !strings.Contains(s, "old-9") || !strings.Contains(s, "dev-x-a1b2c3") {
		t.Errorf("output does not name both the old agent and the new name:\n%s", s)
	}
}

func TestRemoteLookupErrorDoesNotFallThroughToCreate(t *testing.T) {
	isolateHome(t)
	// It must surface and stop.
	reg := &fakeRegistrar{reg: validReg(), findErr: errors.New("connection refused")}
	res, err := Run(context.Background(), Options{Provider: "claude-code", AgentName: "dev-x"},
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
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
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "HALT") || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected HALT with 400, got %v", err)
	}
	if res.Registered {
		t.Error("should not be marked registered on API error")
	}
}

// TestWriteFailureAfterCreateNamesTheOrphan a credential write that fails
// after the agent already exists server-side must name that agent (id and
// name) and say what to do next: re-run init, because its API key and signing
// key were shown exactly once and are now unreachable, and the agent left
// behind holds no usable key.
func TestWriteFailureAfterCreateNamesTheOrphan(t *testing.T) {
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
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("expected an error naming the orphaned agent")
	}
	msg := err.Error()
	for _, want := range []string{"agent-1", "dev-x", "Re-run", "openbox init --provider claude-code", "orphaned"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the failure does not name %q:\n%s", want, msg)
		}
	}
	if !res.Registered {
		t.Error("agent was registered; res.Registered should be true")
	}
	if strings.Contains(msg, "obx_test_SECRETKEYVALUE") || strings.Contains(msg, "PRIVATESEEDVALUE") {
		t.Errorf("error leaked a credential value: %v", err)
	}
}

// TestUnconfirmedIdentityMethodErrors a v3 registration generates its signing
// key locally and never receives one from the backend (only the public JWK
// ever leaves this machine), so the analogous "cannot store runtime
// credentials" failure is the server not confirming a keycloak_workload
// identity at all.
func TestUnconfirmedIdentityMethodErrors(t *testing.T) {
	isolateHome(t)
	r := validReg()
	r.Identity.Method = ""
	reg := &fakeRegistrar{reg: r}
	_, err := Run(context.Background(), Options{Provider: "claude-code", AgentName: "dev-x"},
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), devconfig.IdentityMethodKeycloakWorkload) {
		t.Fatalf("expected an unconfirmed-identity error, got %v", err)
	}
	if kv := readCredentialFile(t); len(kv) != 0 {
		t.Errorf("no credentials should be written when the identity is unconfirmed, got %v", kv)
	}
}

// TestMissingTokenErrors mirrors the above for the other half of "cannot
// store runtime credentials": a confirmed identity with no token to
// authenticate as it.
func TestMissingTokenErrors(t *testing.T) {
	isolateHome(t)
	r := validReg()
	r.APIKey = ""
	reg := &fakeRegistrar{reg: r}
	_, err := Run(context.Background(), Options{Provider: "claude-code", AgentName: "dev-x"},
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), devconfig.IdentityMethodKeycloakWorkload) {
		t.Fatalf("expected an unconfirmed-identity error, got %v", err)
	}
	if kv := readCredentialFile(t); len(kv) != 0 {
		t.Errorf("no credentials should be written when the token is missing, got %v", kv)
	}
}

// TestGenerateKeyFailureErrors a keygen failure must surface named, and must
// never reach Create: the server should not see a request this run cannot
// finish.
func TestGenerateKeyFailureErrors(t *testing.T) {
	isolateHome(t)
	reg := &fakeRegistrar{reg: validReg()}
	_, err := Run(context.Background(), Options{Provider: "claude-code", AgentName: "dev-x"},
		Deps{
			GenerateKey: func() (*rsa.PrivateKey, error) { return nil, errors.New("boom") },
			Registrar:   reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{},
		})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the keygen error to surface, got %v", err)
	}
	if reg.createCalls != 0 {
		t.Errorf("a keygen failure must not reach Create: create=%d", reg.createCalls)
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
		Deps{GenerateKey: fixedGenerateKey, Registrar: &fakeRegistrar{}, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
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
		Deps{GenerateKey: fixedGenerateKey, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
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
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
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

// TestDefaultNameTakenRegistersSuffixed this is what an upgraded machine hits
// when its old agent still exists in the org under the name this install
// would otherwise reuse, or any lost-store re-init: rather than halt, it
// registers under a suffixed name and prints both the old agent (id and name)
// and the new one, so nothing about the recovery route depends on `auth`
// pasting an agent id that this binary no longer has a prompt for.
func TestDefaultNameTakenRegistersSuffixed(t *testing.T) {
	isolateHome(t)
	name := defaultAgentName("claude-code")
	reg := &fakeRegistrar{
		reg:    validReg(),
		byName: map[string]*backend.AgentSummary{name: {ID: "old-9", AgentName: name}},
	}
	var out bytes.Buffer
	res, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &out,
			Suffix: func() (string, error) { return "a1b2c3", nil }})
	if err != nil {
		t.Fatalf("a taken default name must register suffixed, not halt: %v", err)
	}
	wantName := name + "-a1b2c3"
	if reg.lastReq.AgentName != wantName {
		t.Errorf("Create got name %q, want %q", reg.lastReq.AgentName, wantName)
	}
	if reg.createCalls != 1 {
		t.Errorf("Create called %d times, want 1", reg.createCalls)
	}
	if !res.Registered {
		t.Error("res.Registered should be true")
	}
	msg := out.String()
	for _, want := range []string{name, "old-9", wantName} {
		if !strings.Contains(msg, want) {
			t.Errorf("stdout does not name %q:\n%s", want, msg)
		}
	}
}

// TestSuffixedInitReusesOnSecondRun a suffixed registration is written to this
// machine's own store like any other; the second `init` for the same tool
// reuses it offline and never re-suffixes, because it never reaches
// FindByName at all.
func TestSuffixedInitReusesOnSecondRun(t *testing.T) {
	isolateHome(t)
	name := defaultAgentName("claude-code")
	reg := &fakeRegistrar{
		reg:    validReg(),
		byName: map[string]*backend.AgentSummary{name: {ID: "old-9", AgentName: name}},
	}
	deps := Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{},
		Suffix: func() (string, error) { return "a1b2c3", nil }}

	if _, err := Run(context.Background(), Options{Provider: "claude-code"}, deps); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if reg.createCalls != 1 || reg.findCalls != 1 {
		t.Fatalf("first run: creates=%d finds=%d, want 1 and 1", reg.createCalls, reg.findCalls)
	}

	res, err := Run(context.Background(), Options{Provider: "claude-code"}, deps)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !res.Reused || res.Registered {
		t.Errorf("second run: res = %+v, want Reused and not Registered", res)
	}
	if reg.createCalls != 1 || reg.findCalls != 1 {
		t.Errorf("second run: creates=%d finds=%d, want unchanged at 1 and 1 (no re-suffixing)", reg.createCalls, reg.findCalls)
	}
}

// TestCreateDuplicateRetriesOnceWithFreshSuffix the race Create itself can hit
// (another process claims the same suffixed name first) is HTTP 400 with
// "already exists in this organization", not a 409; one retry under a fresh
// suffix, then give up.
func TestCreateDuplicateRetriesOnceWithFreshSuffix(t *testing.T) {
	isolateHome(t)
	name := defaultAgentName("claude-code")
	suffixes := []string{"a1b2c3", "d4e5f6"}
	suffixCalls := 0
	reg := &raceRegistrar{
		reg:          validReg(),
		byName:       map[string]*backend.AgentSummary{name: {ID: "old-9", AgentName: name}},
		duplicateFor: name + "-a1b2c3",
	}
	res, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{},
			Suffix: func() (string, error) {
				s := suffixes[suffixCalls]
				suffixCalls++
				return s, nil
			}})
	if err != nil {
		t.Fatalf("a duplicate-name race must retry once and succeed, got: %v", err)
	}
	if reg.createCalls != 2 {
		t.Errorf("Create called %d times, want 2 (one race, one retry)", reg.createCalls)
	}
	wantName := name + "-d4e5f6"
	if reg.lastReq.AgentName != wantName {
		t.Errorf("the retry used name %q, want %q", reg.lastReq.AgentName, wantName)
	}
	if !res.Registered {
		t.Error("res.Registered should be true after the retry succeeds")
	}
}

// TestCreateDuplicateFailsAfterOneRetry a persistent race (or a name someone
// else is actively claiming) must fail with the backend's own body rather
// than loop.
func TestCreateDuplicateFailsAfterOneRetry(t *testing.T) {
	isolateHome(t)
	reg := &raceRegistrar{reg: validReg(), alwaysDuplicate: true}
	_, err := Run(context.Background(), Options{Provider: "claude-code", AgentName: "dev-x"},
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{},
			Suffix: func() (string, error) { return "a1b2c3", nil }})
	if err == nil {
		t.Fatal("a persistent duplicate-name race must fail, not loop or succeed")
	}
	if reg.createCalls != 2 {
		t.Errorf("Create called %d times, want exactly 2 (one retry, then give up)", reg.createCalls)
	}
	if !strings.Contains(err.Error(), "already exists in this organization") {
		t.Errorf("failure does not carry the backend's own body:\n%v", err)
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
			Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
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
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
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

// TestAnUnscoredAgentPrintsNoTierRow. The control plane leaves tier and trust
// empty until an agent has been scored, and TrustScore reaches here through
// fmt.Sprint of an any -- so "absent" arrives as the literal "<nil>" and the
// row rendered as "tier:   (trust <nil>)". A row with nothing in it is worse
// than no row.
func TestAnUnscoredAgentPrintsNoTierRow(t *testing.T) {
	for _, tc := range []struct {
		name, tier, trust, want string
	}{
		{"scored", "Tier 2", "0.81", "Tier 2 (trust 0.81)"},
		{"tier only", "Tier 2", "<nil>", "Tier 2"},
		{"score only", "", "0.81", "trust 0.81"},
		{"neither", "", "<nil>", ""},
		{"neither, empty score", "", "", ""},
	} {
		if got := tierLabel(tc.tier, tc.trust); got != tc.want {
			t.Errorf("%s: tierLabel(%q, %q) = %q, want %q", tc.name, tc.tier, tc.trust, got, tc.want)
		}
	}

	isolateHome(t)
	var out bytes.Buffer
	reg := &backend.Registration{
		AgentID: "a-1", AgentName: "dev",
		APIKey: "obx_test_k", TrustScore: "<nil>",
		Identity: backend.WorkloadIdentityInfo{Method: devconfig.IdentityMethodKeycloakWorkload, Kid: "kid-a1"},
	}
	if _, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{GenerateKey: fixedGenerateKey, Registrar: &fakeRegistrar{reg: reg}, Installer: &fakeInstaller{}, Out: &out}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s := out.String(); strings.Contains(s, "tier") || strings.Contains(s, "<nil>") {
		t.Errorf("an unscored agent still prints a tier row:\n%s", s)
	}
}

// TestInitRegistersWorkloadAgentWithPublicJWKOnly a fresh registration proves
// possession of a locally-generated key by sending only its PUBLIC half: the
// request carries IdentityVerification{keycloak_workload, generate, openbox,
// <jwk>}, and nothing in the request -- marshaled, not sampled -- contains a
// private key parameter (RFC 7518 "d", or the encoded private key itself).
func TestInitRegistersWorkloadAgentWithPublicJWKOnly(t *testing.T) {
	isolateHome(t)
	reg := &fakeRegistrar{reg: validReg()}

	if _, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	iv := reg.lastReq.IdentityVerification
	if iv == nil {
		t.Fatal("Create request carries no IdentityVerification")
	}
	if iv.Method != devconfig.IdentityMethodKeycloakWorkload {
		t.Errorf("Method = %q, want %q", iv.Method, devconfig.IdentityMethodKeycloakWorkload)
	}
	if iv.Mode != "generate" {
		t.Errorf("Mode = %q, want generate", iv.Mode)
	}
	if iv.SourceType != "openbox" {
		t.Errorf("SourceType = %q, want openbox", iv.SourceType)
	}
	for _, k := range []string{"kty", "n", "e", "kid", "alg", "use"} {
		if iv.PublicJWK[k] == "" {
			t.Errorf("PublicJWK missing %q: %+v", k, iv.PublicJWK)
		}
	}
	if _, hasD := iv.PublicJWK["d"]; hasD {
		t.Error("PublicJWK carries the RFC 7518 private exponent \"d\"")
	}

	raw, err := json.Marshal(reg.lastReq)
	if err != nil {
		t.Fatal(err)
	}
	privB64, err := workloadauth.EncodePrivateKey(fixedTestKey)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), privB64) || strings.Contains(string(raw), "PRIVATE KEY") {
		t.Errorf("the create request leaked private key material: %s", raw)
	}
}

// TestSecondInitReusesWithoutRegistering across two separate Run invocations
// against the SAME store, Create and the key generator each fire exactly
// once (on the first run); the second reuses offline, and the credential
// file's bytes are identical before and after it.
func TestSecondInitReusesWithoutRegistering(t *testing.T) {
	isolateHome(t)
	reg := &fakeRegistrar{reg: validReg()}
	keygenCalls := 0
	countingKeygen := func() (*rsa.PrivateKey, error) {
		keygenCalls++
		return fixedGenerateKey()
	}

	if _, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{GenerateKey: countingKeygen, Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if reg.createCalls != 1 || keygenCalls != 1 {
		t.Fatalf("first run: creates=%d keygenCalls=%d, want 1 and 1", reg.createCalls, keygenCalls)
	}
	envBefore := readCredentialFile(t)

	res, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{GenerateKey: countingKeygen, Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !res.Reused || res.Registered {
		t.Errorf("second run: res = %+v, want Reused and not Registered", res)
	}
	if reg.createCalls != 1 || keygenCalls != 1 {
		t.Errorf("second run: creates=%d keygenCalls=%d, want unchanged at 1 and 1", reg.createCalls, keygenCalls)
	}
	envAfter := readCredentialFile(t)
	if envBefore[devconfig.EnvAPIKeyDirect] != envAfter[devconfig.EnvAPIKeyDirect] ||
		envBefore[devconfig.EnvWorkloadPrivateKey] != envAfter[devconfig.EnvWorkloadPrivateKey] {
		t.Errorf("credential file changed across the reuse run: before=%v after=%v", envBefore, envAfter)
	}
}

// TestInitTranslatesCreateConflicts the three known create-time 409 classes
// each get a message a developer can act on; the generic aivss HALT wording
// never appears for them. An unrecognized 409 still falls to that generic
// text.
func TestInitTranslatesCreateConflicts(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantSubstr string
		wantHalt   bool
	}{
		{
			name:       "idp not initialized",
			body:       `{"message":"The organization identity provider must be initialized before creating a workload-authenticated agent"}`,
			wantSubstr: "ask your OpenBox admin",
		},
		{
			name:       "external idp",
			body:       `{"message":"The selected identity does not belong to the active identity provider"}`,
			wantSubstr: "external provider (Okta/Entra)",
		},
		{
			name:       "idp changed",
			body:       `{"message":"The active identity provider changed during agent registration"}`,
			wantSubstr: "changed during registration",
		},
		{
			name:     "unknown 409 falls to the generic HALT",
			body:     `{"message":"The selected workload identity is not active, verified, and eligible"}`,
			wantHalt: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateHome(t)
			reg := &fakeRegistrar{createErr: &backend.APIError{StatusCode: 409, Body: tt.body}}
			_, err := Run(context.Background(), Options{Provider: "claude-code", AgentName: "dev-x"},
				Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
			if err == nil {
				t.Fatal("expected an error")
			}
			msg := err.Error()
			if tt.wantHalt {
				if !strings.Contains(msg, "HALT: agent/create") {
					t.Errorf("unknown 409 lost the generic HALT text: %s", msg)
				}
				return
			}
			if strings.Contains(msg, "HALT: agent/create") {
				t.Errorf("a known 409 class still shows the generic HALT: %s", msg)
			}
			if !strings.Contains(msg, tt.wantSubstr) {
				t.Errorf("message = %q, want it to contain %q", msg, tt.wantSubstr)
			}
		})
	}
}

// TestLegacyNoticePrintsOnceBeforeRegistering a legacy store means hooks have
// been sending nothing; that has to be said once, before the registration
// that replaces it, not folded silently into the success output.
func TestLegacyNoticePrintsOnceBeforeRegistering(t *testing.T) {
	dir := isolateHome(t)
	if err := devconfig.SetLegacyDID(filepath.Join(dir, "claude-code", "dev.json"), "did:aip:existing"); err != nil {
		t.Fatal(err)
	}

	reg := &fakeRegistrar{reg: validReg()}
	var out bytes.Buffer
	if _, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &out}); err != nil {
		t.Fatalf("run: %v", err)
	}
	s := out.String()
	if n := strings.Count(s, "legacy (pre-IAMv3) identity"); n != 1 {
		t.Errorf("legacy notice printed %d times, want exactly 1:\n%s", n, s)
	}
}

// TestInitReplacesLegacyStoreCleanly the end state of registering over a
// legacy store has no DID, no seed, and a v3 identity_method; the second init
// afterward reuses it silently (no re-registration, no repeated notice).
func TestInitReplacesLegacyStoreCleanly(t *testing.T) {
	dir := isolateHome(t)
	envPath, err := devconfig.EnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteEnvFile(envPath, map[string]string{
		devconfig.EnvAPIKeyDirect:    "obx_legacy",
		devconfig.EnvAgentPrivateKey: "legacyseed",
	}); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "claude-code", "dev.json")
	if err := devconfig.SetLegacyDID(cfgPath, "did:aip:legacy"); err != nil {
		t.Fatal(err)
	}

	reg := &fakeRegistrar{reg: validReg()}
	res, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Registered {
		t.Fatal("expected a fresh registration over the legacy store")
	}

	kv := readCredentialFile(t)
	if _, ok := kv[devconfig.EnvAgentPrivateKey]; ok {
		t.Errorf("legacy seed survived the write: %v", kv)
	}
	cfg, err := devconfig.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DID != "" {
		t.Errorf("DID survived the write: %q", cfg.DID)
	}
	if cfg.IdentityMethod != devconfig.IdentityMethodKeycloakWorkload {
		t.Errorf("identity_method = %q, want %q", cfg.IdentityMethod, devconfig.IdentityMethodKeycloakWorkload)
	}

	res2, err := Run(context.Background(), Options{Provider: "claude-code"},
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !res2.Reused || res2.Registered {
		t.Errorf("second run: res = %+v, want Reused and not Registered", res2)
	}
	if reg.createCalls != 1 {
		t.Errorf("Create called %d times across both runs, want 1", reg.createCalls)
	}
}

// TestRegisteredKidMustMatchSubmittedJWK a backend that persisted a different
// key than the one this machine proved possession of would fail exchange on
// every hook call, silently (fail-open); the mismatch must be caught before
// any credential is written, not discovered later at exchange.
func TestRegisteredKidMustMatchSubmittedJWK(t *testing.T) {
	isolateHome(t)
	r := validReg()
	r.Identity.Kid = "kid-from-a-different-key"
	reg := &fakeRegistrar{reg: r, kidMismatch: true}
	_, err := Run(context.Background(), Options{Provider: "claude-code", AgentName: "dev-x"},
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected a kid-mismatch refusal, got %v", err)
	}
	if kv := readCredentialFile(t); len(kv) != 0 {
		t.Errorf("no credentials should be written on a kid mismatch, got %v", kv)
	}
}
