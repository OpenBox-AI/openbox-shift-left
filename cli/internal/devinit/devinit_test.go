package devinit

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/backend"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/provider"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/secret"
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

// failStore fails Set for accounts containing failAcct; everything else defers
// to the embedded MemStore.
type failStore struct {
	*secret.MemStore
	failAcct string
}

func (s *failStore) Set(service, account, value string) error {
	if strings.Contains(account, s.failAcct) {
		return errors.New("keyring locked")
	}
	return s.MemStore.Set(service, account, value)
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
	reg := &fakeRegistrar{}
	store := secret.NewMemStore()
	inst := &fakeInstaller{avail: false}
	var out bytes.Buffer

	_, err := Run(context.Background(), Options{Provider: "claude-code", Org: "acme", DryRun: true},
		Deps{Registrar: reg, Store: store, Installer: inst, Out: &out})
	if err != nil {
		t.Fatalf("dry-run err: %v", err)
	}
	if reg.createCalls != 0 || reg.findCalls != 0 {
		t.Errorf("dry-run touched the network: create=%d find=%d", reg.createCalls, reg.findCalls)
	}
	if store.Len() != 0 {
		t.Errorf("dry-run wrote %d secrets, want 0", store.Len())
	}
	if !strings.Contains(out.String(), "DRY RUN") {
		t.Errorf("dry-run output missing DRY RUN banner:\n%s", out.String())
	}
}

func TestDryRunDisclosesInstallGitHook(t *testing.T) {
	var out bytes.Buffer
	_, err := Run(context.Background(),
		Options{Provider: "claude-code", Org: "acme", DryRun: true, InstallGitHook: true},
		Deps{Registrar: &fakeRegistrar{}, Store: secret.NewMemStore(), Installer: &fakeInstaller{avail: false}, Out: &out})
	if err != nil {
		t.Fatalf("dry-run err: %v", err)
	}
	if !strings.Contains(out.String(), "install_git_hook: true") {
		t.Errorf("dry-run must disclose the ambient commit-hook install:\n%s", out.String())
	}
}

func TestHappyPathStoresCredsNeverPrintsThem(t *testing.T) {
	reg := &fakeRegistrar{reg: validReg()}
	store := secret.NewMemStore()
	inst := &fakeInstaller{avail: false} // Claude Code adapter (SL-4) not built
	var out bytes.Buffer

	res, err := Run(context.Background(), Options{Provider: "claude-code", Org: "acme"},
		Deps{Registrar: reg, Store: store, Installer: inst, Out: &out})

	// Registration + credential capture succeed; config is manual-only → error.
	if err == nil || !res.ConfigManualOnly {
		t.Fatalf("expected manual-config error, got err=%v res=%+v", err, res)
	}
	if !res.Registered || res.AgentID != "agent-1" {
		t.Errorf("res = %+v", res)
	}
	// Three secrets stored.
	if store.Len() != 3 {
		t.Fatalf("stored %d secrets, want 3", store.Len())
	}
	svc, apiAcct, privAcct, didAcct := Options{Provider: "claude-code", Org: "acme"}.accounts()
	if v, _ := store.Get(svc, apiAcct); v != "obx_test_SECRETKEYVALUE" {
		t.Errorf("api key not stored, got %q", v)
	}
	if v, _ := store.Get(svc, privAcct); v != "PRIVATESEEDVALUE" {
		t.Errorf("private key not stored, got %q", v)
	}
	if v, _ := store.Get(svc, didAcct); v != "did:aip:abc" {
		t.Errorf("did not stored, got %q", v)
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
	reg := &fakeRegistrar{reg: validReg()}
	inst := &fakeInstaller{avail: true}
	res, err := Run(context.Background(), Options{Provider: "claude-code", Org: "acme"},
		Deps{Registrar: reg, Store: secret.NewMemStore(), Installer: inst, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.ConfigApplied || !inst.installed {
		t.Errorf("config not applied: res=%+v installed=%v", res, inst.installed)
	}
	if inst.gotRef.DID != "did:aip:abc" || inst.gotRef.APIKeyAccount == "" {
		t.Errorf("installer got bad ref: %+v", inst.gotRef)
	}
}

func TestIdempotentReuseSkipsRegistration(t *testing.T) {
	store := secret.NewMemStore()
	svc, apiAcct, privAcct, didAcct := Options{Provider: "claude-code", Org: "acme"}.accounts()
	_ = store.Set(svc, apiAcct, "obx_test_existing")
	_ = store.Set(svc, privAcct, "existingseed")
	_ = store.Set(svc, didAcct, "did:aip:existing")

	reg := &fakeRegistrar{reg: validReg()}
	inst := &fakeInstaller{avail: true}
	res, err := Run(context.Background(), Options{Provider: "claude-code", Org: "acme"},
		Deps{Registrar: reg, Store: store, Installer: inst, Out: &bytes.Buffer{}})
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
	reg := &fakeRegistrar{
		reg:    validReg(),
		byName: map[string]*backend.AgentSummary{"dev-x": {ID: "old-9", DID: "did:aip:old"}},
	}
	res, err := Run(context.Background(), Options{Provider: "claude-code", Org: "acme", AgentName: "dev-x"},
		Deps{Registrar: reg, Store: secret.NewMemStore(), Installer: &fakeInstaller{avail: true}, Out: &bytes.Buffer{}})
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
	// F1: a failed agent/list must NOT silently proceed to Create (which would
	// bypass idempotent detection). It must surface and stop.
	reg := &fakeRegistrar{reg: validReg(), findErr: errors.New("connection refused")}
	res, err := Run(context.Background(), Options{Provider: "claude-code", Org: "acme", AgentName: "dev-x"},
		Deps{Registrar: reg, Store: secret.NewMemStore(), Installer: &fakeInstaller{avail: true}, Out: &bytes.Buffer{}})
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
	// F4: under --force, a failed lookup must surface rather than proceed to a
	// confusing 400 with a name that may be taken.
	reg := &fakeRegistrar{reg: validReg(), findErr: errors.New("connection refused")}
	_, err := Run(context.Background(), Options{Provider: "claude-code", Org: "acme", AgentName: "dev-x", Force: true},
		Deps{Registrar: reg, Store: secret.NewMemStore(), Installer: &fakeInstaller{avail: true}, Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "free agent name") {
		t.Fatalf("expected free-name lookup error, got %v", err)
	}
	if reg.createCalls != 0 {
		t.Errorf("must not create when the free-name lookup failed: create=%d", reg.createCalls)
	}
}

func TestForceRegistersUnderFreeName(t *testing.T) {
	reg := &fakeRegistrar{
		reg:    validReg(),
		byName: map[string]*backend.AgentSummary{"dev-x": {ID: "old-9"}}, // dev-x-2 is free
	}
	inst := &fakeInstaller{avail: true}
	_, err := Run(context.Background(), Options{Provider: "claude-code", Org: "acme", AgentName: "dev-x", Force: true},
		Deps{Registrar: reg, Store: secret.NewMemStore(), Installer: inst, Out: &bytes.Buffer{}})
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
	reg := &fakeRegistrar{createErr: &backend.APIError{StatusCode: 400, Body: "AIVSS config is required"}}
	res, err := Run(context.Background(), Options{Provider: "claude-code", Org: "acme", AgentName: "dev-x"},
		Deps{Registrar: reg, Store: secret.NewMemStore(), Installer: &fakeInstaller{avail: true}, Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "HALT") || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected HALT with 400, got %v", err)
	}
	if res.Registered {
		t.Error("should not be marked registered on API error")
	}
}

func TestPartialFailureReportsAgentAndResume(t *testing.T) {
	store := &failStore{MemStore: secret.NewMemStore(), failAcct: "private_key"}
	reg := &fakeRegistrar{reg: validReg()}
	res, err := Run(context.Background(), Options{Provider: "claude-code", Org: "acme", AgentName: "dev-x"},
		Deps{Registrar: reg, Store: store, Installer: &fakeInstaller{avail: true}, Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "agent-1") || !strings.Contains(err.Error(), "rotate") {
		t.Fatalf("expected resume guidance naming agent-1, got %v", err)
	}
	if !res.Registered {
		t.Error("agent was registered; res.Registered should be true")
	}
	// API key stored before the failing private-key write.
	svc, apiAcct, privAcct, _ := Options{Provider: "claude-code", Org: "acme"}.accounts()
	if v, _ := store.Get(svc, apiAcct); v == "" {
		t.Error("api key should have been stored before the failure")
	}
	if _, err := store.Get(svc, privAcct); err != secret.ErrNotFound {
		t.Error("private key should not be stored after the failed Set")
	}
}

func TestMissingPrivateKeyErrors(t *testing.T) {
	r := validReg()
	r.PrivateKey = ""
	store := secret.NewMemStore()
	reg := &fakeRegistrar{reg: r}
	_, err := Run(context.Background(), Options{Provider: "claude-code", Org: "acme", AgentName: "dev-x"},
		Deps{Registrar: reg, Store: store, Installer: &fakeInstaller{avail: true}, Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "signing key") {
		t.Fatalf("expected signing-key error, got %v", err)
	}
	if store.Len() != 0 {
		t.Errorf("no creds should be stored when the key is missing, got %d", store.Len())
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
	_, err := Run(context.Background(), Options{Provider: ""},
		Deps{Registrar: &fakeRegistrar{}, Store: secret.NewMemStore(), Installer: &fakeInstaller{}, Out: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("expected error for empty provider")
	}
}
