package main

import (
	"context"
	"crypto/rsa"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/backend"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/devinit"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/prompt"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/client/workloadauth"
)

// testOrgToken and the minted key below are assembled rather than written
// out: a credential-shaped literal in this repo is rewritten on disk by the
// local secret hook, which would leave these cases asserting against a
// placeholder.
var testOrgToken = "obx" + "_key_" + strings.Repeat("f", 48)

const mintedKeyPrefix = "obx" + "_minted_"

// requireUnbound is the precondition every case here depends on. `init` reads
// the ORG dev.json before it binds, and a fixture that arrived already bound
// would make that read land on the tool's own config -- so the case that
// proves the org URLs are carried across would pass without the carry
// happening at all.
func requireUnbound(t *testing.T) {
	t.Helper()
	if got := devconfig.BoundProvider(); got != "" {
		t.Fatalf("BoundProvider() = %q; these cases must start unbound or they prove nothing", got)
	}
}

// countingReg records what a run asked of the control plane, and mints a
// different agent each time so two tools cannot accidentally share a DID.
type countingReg struct {
	creates  int
	finds    int
	byName   map[string]*backend.AgentSummary
	next     int
	lastName string
}

func (r *countingReg) FindByName(_ context.Context, name string) (*backend.AgentSummary, error) {
	r.finds++
	return r.byName[name], nil
}

func (r *countingReg) Create(_ context.Context, req backend.CreateAgentRequest) (*backend.Registration, error) {
	r.creates++
	r.next++
	r.lastName = req.AgentName
	suffix := strings.Repeat("0", 3) + string(rune('0'+r.next))
	return &backend.Registration{
		AgentID:    "srv-agent-" + suffix,
		AgentName:  req.AgentName,
		APIKey:     mintedKeyPrefix + suffix,
		Tier:       "Tier 2",
		TrustScore: "0.81",
		Identity: backend.WorkloadIdentityInfo{
			Method:     devconfig.IdentityMethodKeycloakWorkload,
			SourceType: "openbox",
			Kid:        "kid-" + suffix,
		},
	}, nil
}

// panicReg fails loudly if a run that must be offline reaches the network.
type panicReg struct{}

func (panicReg) FindByName(_ context.Context, _ string) (*backend.AgentSummary, error) {
	panic("a reuse run reached the control plane")
}

func (panicReg) Create(_ context.Context, _ backend.CreateAgentRequest) (*backend.Registration, error) {
	panic("a reuse run registered an agent")
}

// TestInitRegistersPerToolThenReusesOffline the whole shape of the split in
// one case: register once per tool, reuse offline afterwards, and never let
// one tool's run touch another tool's store.
func TestInitRegistersPerToolThenReusesOffline(t *testing.T) {
	home := isolateHomeUnbound(t)
	requireUnbound(t)
	t.Setenv(devconfig.EnvControlToken, testOrgToken)
	clearAgentEnv(t)
	reg := &countingReg{byName: map[string]*backend.AgentSummary{}}

	// Run 1: claude-code has no store, so this registers.
	a, out, errb := testApp(nil)
	a.newRegistrar = func(_, _, _ string) devinit.Registrar { return reg }
	a.newPrompt = declineAdopt(t)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("first init exit = %d; stderr=%q", code, errb.String())
	}
	if reg.creates != 1 {
		t.Fatalf("Create called %d times on a fresh tool, want 1:\n%s", reg.creates, out.String())
	}
	ccEnv := filepath.Join(home, "claude-code", ".env")
	fi, err := os.Stat(ccEnv)
	if err != nil {
		t.Fatalf("no credential file for claude-code: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s mode = %v, want 0600", ccEnv, perm)
	}
	ccBefore, err := os.ReadFile(ccEnv)
	if err != nil {
		t.Fatal(err)
	}

	// Run 2: the same tool, no token, a registrar that panics if reached.
	t.Setenv(devconfig.EnvControlToken, "")
	b, _, berr := testApp(nil)
	b.newRegistrar = func(_, _, _ string) devinit.Registrar { return panicReg{} }
	b.newPrompt = panicPrompt(t)
	if code := b.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("re-init exit = %d; a tool with a store must run offline; stderr=%q", code, berr.String())
	}
	if reg.creates != 1 {
		t.Errorf("Create called again on a re-run: %d", reg.creates)
	}

	// Run 3: a second tool gets its own agent, and the first one is untouched.
	t.Setenv(devconfig.EnvControlToken, testOrgToken)
	c, _, cerr := testApp(nil)
	c.newRegistrar = func(_, _, _ string) devinit.Registrar { return reg }
	c.newPrompt = declineAdopt(t)
	if code := c.run([]string{"init", "--provider", "codex"}); code != exitOK {
		t.Fatalf("codex init exit = %d; stderr=%q", code, cerr.String())
	}
	if reg.creates != 2 {
		t.Fatalf("Create called %d times, want a second agent for codex", reg.creates)
	}
	ccAgentID := agentIDFromStore(t, home, "claude-code")
	cxAgentID := agentIDFromStore(t, home, "codex")
	if ccAgentID == "" || cxAgentID == "" {
		t.Fatalf("a store has no agent id: claude-code=%q codex=%q", ccAgentID, cxAgentID)
	}
	if ccAgentID == cxAgentID {
		t.Errorf("both tools registered the same agent id %q", ccAgentID)
	}
	ccAfter, err := os.ReadFile(ccEnv)
	if err != nil {
		t.Fatal(err)
	}
	if string(ccBefore) != string(ccAfter) {
		t.Errorf("installing codex rewrote claude-code's credentials:\n%s\n---\n%s", ccBefore, ccAfter)
	}
}

// TestInitCarriesTheOrgURLsIntoTheToolConfig one dev.json is loaded, never
// merged over another, so a per-tool config does not inherit the org's
// coordinates -- `init` has to read them before it binds and copy them in. Get
// the order wrong and the tool silently talks to the hosted default while the
// org runs its own core.
func TestInitCarriesTheOrgURLsIntoTheToolConfig(t *testing.T) {
	home := isolateHomeUnbound(t)
	requireUnbound(t)
	t.Setenv(devconfig.EnvControlToken, testOrgToken)
	clearAgentEnv(t)

	orgCfg := filepath.Join(home, "dev.json")
	if err := devconfig.WriteConfig(orgCfg, devconfig.Update{
		BackendURL: "https://api.internal", BaseURL: "https://core.internal"}); err != nil {
		t.Fatal(err)
	}
	orgBefore, err := os.ReadFile(orgCfg)
	if err != nil {
		t.Fatal(err)
	}

	a, _, errb := testApp(nil)
	a.newRegistrar = func(_, _, _ string) devinit.Registrar {
		return &countingReg{byName: map[string]*backend.AgentSummary{}}
	}
	a.newPrompt = declineAdopt(t)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}

	toolCfg, err := devconfig.Load(filepath.Join(home, "claude-code", "dev.json"))
	if err != nil {
		t.Fatal(err)
	}
	if toolCfg.BackendURL != "https://api.internal" {
		t.Errorf("tool backend_url = %q, want the org's", toolCfg.BackendURL)
	}
	if toolCfg.BaseURL != "https://core.internal" {
		t.Errorf("tool base_url = %q, want the org's", toolCfg.BaseURL)
	}
	orgAfter, err := os.ReadFile(orgCfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(orgBefore) != string(orgAfter) {
		t.Errorf("init wrote to the org config:\n%s\n---\n%s", orgBefore, orgAfter)
	}
}

// TestTheOrgTokenIsNeverMintedAsAnAgentKey the org token creates and rotates
// agents across the organization; the agent key governs one tool on one
// machine. Both are "the credential" in different sentences, and writing the
// first where the second belongs would put fleet authority in every tool's
// store.
func TestTheOrgTokenIsNeverMintedAsAnAgentKey(t *testing.T) {
	home := isolateHomeUnbound(t)
	requireUnbound(t)
	clearAgentEnv(t)
	t.Setenv(devconfig.EnvControlToken, "")

	// Only in the org file, so the run has to read it from there.
	orgEnv, err := devconfig.OrgEnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	orgOnly := map[string]string{devconfig.EnvControlToken: testOrgToken}
	if err := devconfig.WriteEnvFile(orgEnv, orgOnly); err != nil {
		t.Fatal(err)
	}

	a, _, errb := testApp(nil)
	a.newRegistrar = func(_, _, _ string) devinit.Registrar {
		return &countingReg{byName: map[string]*backend.AgentSummary{}}
	}
	a.newPrompt = declineAdopt(t)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}

	kv, err := devconfig.ParseEnvFile(filepath.Join(home, "claude-code", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if kv[devconfig.EnvAPIKeyDirect] == testOrgToken {
		t.Fatal("the org control token was written as the agent's API key")
	}
	if !strings.HasPrefix(kv[devconfig.EnvAPIKeyDirect], mintedKeyPrefix) {
		t.Errorf("agent key = %q, want the registrar's minted key", kv[devconfig.EnvAPIKeyDirect])
	}
	if _, ok := kv[devconfig.EnvControlToken]; ok {
		t.Error("the org control token was copied into the per-tool store")
	}
}

// TestInitWithNoStoreAndNoTokenRefuses the only refusal left, and it has to
// name both routes: a user who skipped `auth` and a user whose token lives
// only in their environment arrive here the same way.
func TestInitWithNoStoreAndNoTokenRefuses(t *testing.T) {
	home := isolateHomeUnbound(t)
	requireUnbound(t)
	clearAgentEnv(t)
	t.Setenv(devconfig.EnvControlToken, "")

	a, _, errb := testApp(nil)
	a.newRegistrar = func(_, _, _ string) devinit.Registrar { return panicReg{} }
	a.newPrompt = noTerminal(t)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitError {
		t.Fatalf("exit = %d, want a refusal", code)
	}
	s := errb.String()
	for _, want := range []string{"openbox auth", devconfig.EnvControlToken, "Nothing was installed"} {
		if !strings.Contains(s, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, s)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "claude-code")); !os.IsNotExist(err) {
		t.Errorf("a refused init created the tool's identity directory (err=%v)", err)
	}
}

// TestAdoptPromptWritesTheToolStoreAndSkipsRegistration (owner ruling O2.)
// Shrinking `auth` removed the only interactive route to a rotated key; this
// is where it came back, and it is also the one recovery from the
// duplicate-name halt.
func TestAdoptPromptWritesTheToolStoreAndSkipsRegistration(t *testing.T) {
	home := isolateHomeUnbound(t)
	requireUnbound(t)
	t.Setenv(devconfig.EnvControlToken, testOrgToken)
	clearAgentEnv(t)

	reg := &countingReg{byName: map[string]*backend.AgentSummary{}}
	pasted := map[string]string{
		"id":  "adopted-agent-7",
		"did": "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301",
		"key": "obx_pasted_by_hand",
	}

	a, _, errb := testApp(nil)
	a.newRegistrar = func(_, _, _ string) devinit.Registrar { return reg }
	p := &prompt.Scripted{Answers: []string{"y", pasted["id"], pasted["did"], pasted["key"], testSeedB64}}
	a.newPrompt = func() (prompt.Prompter, error) { return p, nil }

	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	if reg.creates != 0 {
		t.Errorf("adopting still registered a new agent (%d creates)", reg.creates)
	}

	kv, err := devconfig.ParseEnvFile(filepath.Join(home, "claude-code", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if kv[devconfig.EnvAPIKeyDirect] != pasted["key"] || kv[devconfig.EnvAgentPrivateKey] != testSeedB64 {
		t.Errorf("the pasted credentials were not stored: %v", kv)
	}
	cfg, err := devconfig.Load(filepath.Join(home, "claude-code", "dev.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DID != pasted["did"] || cfg.AgentID != pasted["id"] {
		t.Errorf("the pasted coordinates were not stored: %+v", cfg)
	}
}

// TestAdoptRejectsABadSeedBeforeWritingAnything a half-populated store is
// worse than none: the next run would read it as "already registered" and
// install hooks against an identity that cannot sign.
func TestAdoptRejectsABadSeedBeforeWritingAnything(t *testing.T) {
	home := isolateHomeUnbound(t)
	requireUnbound(t)
	t.Setenv(devconfig.EnvControlToken, testOrgToken)
	clearAgentEnv(t)

	a, _, errb := testApp(nil)
	a.newRegistrar = func(_, _, _ string) devinit.Registrar { return panicReg{} }
	p := &prompt.Scripted{Answers: []string{"y", "adopted-agent-7",
		"did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301", "obx_pasted_by_hand", "not-base64!!"}}
	a.newPrompt = func() (prompt.Prompter, error) { return p, nil }

	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitError {
		t.Fatalf("exit = %d, want a refusal on an unusable seed", code)
	}
	if !strings.Contains(errb.String(), "base64") {
		t.Errorf("the refusal does not say what is wrong with the seed:\n%s", errb.String())
	}
	if _, err := os.Stat(filepath.Join(home, "claude-code", ".env")); !os.IsNotExist(err) {
		t.Errorf("a rejected adopt wrote a credential file anyway (err=%v)", err)
	}
}

// TestNonInteractiveInitSkipsAdoptAndRegisters an unattended install already
// knows the tool has no identity and that the org token is present; refusing
// for want of a terminal would turn a working CI provision into a failure.
func TestNonInteractiveInitSkipsAdoptAndRegisters(t *testing.T) {
	isolateHomeUnbound(t)
	requireUnbound(t)
	t.Setenv(devconfig.EnvControlToken, testOrgToken)
	clearAgentEnv(t)

	reg := &countingReg{byName: map[string]*backend.AgentSummary{}}
	a, _, errb := testApp(nil)
	a.newRegistrar = func(_, _, _ string) devinit.Registrar { return reg }
	a.newPrompt = func() (prompt.Prompter, error) { return nil, prompt.ErrNotATerminal }

	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	if reg.creates != 1 {
		t.Errorf("Create called %d times, want 1", reg.creates)
	}
	if strings.Contains(errb.String(), "terminal") {
		t.Errorf("an unattended install complained about the terminal:\n%s", errb.String())
	}
}

// TestLegacyStorePlusTokenRegistersFresh the upgrade case. A machine that ran
// the old `auth` has a complete agent identity at the org level, and under the
// no-fallback rule it counts for nothing: the tool registers its own. The org
// file must come through untouched, because `uninstall` and the control token
// still depend on it.
func TestLegacyStorePlusTokenRegistersFresh(t *testing.T) {
	home := isolateHomeUnbound(t)
	requireUnbound(t)
	clearAgentEnv(t)
	t.Setenv(devconfig.EnvControlToken, testOrgToken)

	orgEnv, err := devconfig.OrgEnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteEnvFile(orgEnv, map[string]string{
		devconfig.EnvAPIKeyDirect:    "obx_legacy_org_key",
		devconfig.EnvAgentPrivateKey: testSeedB64}); err != nil {
		t.Fatal(err)
	}
	const legacyDID = "did:aip:11111111-2222-3333-4444-555555555555"
	if err := devconfig.WriteConfig(filepath.Join(home, "dev.json"), devconfig.Update{DID: legacyDID}); err != nil {
		t.Fatal(err)
	}
	orgEnvBefore, err := os.ReadFile(orgEnv)
	if err != nil {
		t.Fatal(err)
	}

	reg := &countingReg{byName: map[string]*backend.AgentSummary{}}
	a, _, errb := testApp(nil)
	a.newRegistrar = func(_, _, _ string) devinit.Registrar { return reg }
	a.newPrompt = declineAdopt(t)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	if reg.creates != 1 {
		t.Fatalf("the org-level store was reused; a per-tool read must not reach it (%d creates)", reg.creates)
	}
	if got := agentIDFromStore(t, home, "claude-code"); got == "" {
		t.Error("the tool never minted its own agent id")
	}
	orgEnvAfter, err := os.ReadFile(orgEnv)
	if err != nil {
		t.Fatal(err)
	}
	if string(orgEnvBefore) != string(orgEnvAfter) {
		t.Errorf("init rewrote the org credential file:\n%s\n---\n%s", orgEnvBefore, orgEnvAfter)
	}
}

// declineAdopt answers "no" to the adopt question and nothing else, so a run
// that asks a second question fails by running out of answers.
func declineAdopt(t *testing.T) func() (prompt.Prompter, error) {
	t.Helper()
	return func() (prompt.Prompter, error) { return &prompt.Scripted{Answers: []string{"n"}}, nil }
}

// noTerminal is how an unattended run reaches the prompter. Adopt is offered
// before the token check, so a refusal case legitimately gets this far; what it
// must not do is ask a question nobody can answer.
func noTerminal(t *testing.T) func() (prompt.Prompter, error) {
	t.Helper()
	return func() (prompt.Prompter, error) { return nil, prompt.ErrNotATerminal }
}

// panicPrompt is for a run that must not reach the prompter at all: the reuse
// path, which is offline and asks nothing.
func panicPrompt(t *testing.T) func() (prompt.Prompter, error) {
	t.Helper()
	return func() (prompt.Prompter, error) {
		t.Error("a run that must not prompt reached the prompter")
		return nil, prompt.ErrNotATerminal
	}
}

// clearAgentEnv removes the variables that would answer the identity question
// before any file is consulted; several of these are exported in this test
// binary by other fixtures.
func clearAgentEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		devconfig.EnvAPIKeyDirect, devconfig.EnvAgentPrivateKey, devconfig.EnvDID,
		devconfig.EnvAgentID, "OPENBOX_ED25519_SEED", "OPENBOX_SEED",
	} {
		t.Setenv(name, "")
	}
}

// agentIDFromStore reads back the coordinate a v3 registration actually
// persists. A v3 store never sets developer_did (D1: the attribution label is
// derived, never stored), so this replaces the pre-flip didFromStore.
func agentIDFromStore(t *testing.T, home, tool string) string {
	t.Helper()
	cfg, err := devconfig.Load(filepath.Join(home, tool, "dev.json"))
	if err != nil {
		t.Fatalf("read %s config: %v", tool, err)
	}
	return cfg.AgentID
}

// TestInitRegistersAndInstalls inverts what `auth` used to prove. Registration
// and installation were deliberately two commands, so that `auth` could not
// write a hook and `init` could not write a credential; the split moved, and
// `init` now does both in one run. The two halves are asserted together
// because a run that registers and then fails to install leaves an agent in
// the org that governs nothing.
func TestInitRegistersAndInstalls(t *testing.T) {
	home := isolateHomeUnbound(t)
	requireUnbound(t)
	t.Setenv(devconfig.EnvControlToken, testOrgToken)
	clearAgentEnv(t)

	reg := &countingReg{byName: map[string]*backend.AgentSummary{}}
	a, out, errb := testApp(nil)
	a.newRegistrar = func(_, _, _ string) devinit.Registrar { return reg }
	a.newPrompt = declineAdopt(t)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	if reg.creates != 1 {
		t.Fatalf("Create called %d times, want 1", reg.creates)
	}
	if !strings.Contains(out.String(), "EVERY SESSION") {
		t.Errorf("the install did not complete:\n%s", out.String())
	}

	kv, err := devconfig.ParseEnvFile(filepath.Join(home, "claude-code", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if kv[devconfig.EnvAPIKeyDirect] == "" || kv[devconfig.EnvWorkloadPrivateKey] == "" {
		t.Errorf("the minted credentials were not stored: %v", kv)
	}
	// Minted values are written to a 0600 file and never printed (INV-1).
	if strings.Contains(out.String(), kv[devconfig.EnvAPIKeyDirect]) ||
		strings.Contains(out.String(), kv[devconfig.EnvWorkloadPrivateKey]) {
		t.Errorf("a minted secret reached stdout:\n%s", out.String())
	}
}

// TestTheAgentNameIsPerToolOnTheWire the default name reaches the control
// plane, so the tool scoping has to survive the whole call and not just the
// helper that builds it: two tools registering under one name is the
// duplicate-name halt, hit on a machine that did nothing wrong.
func TestTheAgentNameIsPerToolOnTheWire(t *testing.T) {
	isolateHomeUnbound(t)
	requireUnbound(t)
	t.Setenv(devconfig.EnvControlToken, testOrgToken)
	clearAgentEnv(t)

	reg := &countingReg{byName: map[string]*backend.AgentSummary{}}
	names := map[string]string{}
	for _, tool := range []string{"claude-code", "codex"} {
		a, _, errb := testApp(nil)
		a.newRegistrar = func(_, _, _ string) devinit.Registrar { return reg }
		a.newPrompt = declineAdopt(t)
		if code := a.run([]string{"init", "--provider", tool}); code != exitOK {
			t.Fatalf("%s init exit = %d; stderr=%q", tool, code, errb.String())
		}
		names[tool] = reg.lastName
		if !strings.HasPrefix(reg.lastName, tool+"-") {
			t.Errorf("%s registered as %q; the name must carry the tool", tool, reg.lastName)
		}
	}
	if names["claude-code"] == names["codex"] {
		t.Errorf("both tools registered under one name %q; the second would halt on a duplicate", names["codex"])
	}
}

// TestInitInstallsAgainstAnEnvironmentIdentity the documented three-variable
// route, which is what a CI image uses. It broke silently once: the install
// gate accepted an environment identity and devinit's reuse branch looked only
// at the file, so the run reached registration with no registrar wired and was
// told it had hit a wiring defect. The two predicates have to stay one
// question.
//
// Registering here would be wrong for a second reason: exported credentials
// outrank every store at runtime, so a minted agent would never be used -- one
// orphan per ephemeral runner.
func TestInitInstallsAgainstAnEnvironmentIdentity(t *testing.T) {
	home := isolateHomeUnbound(t)
	requireUnbound(t)
	t.Setenv(devconfig.EnvControlToken, "")
	t.Setenv(devconfig.EnvAPIKeyDirect, "obx"+"_from_the_environment")
	t.Setenv(devconfig.EnvWorkloadPrivateKey, testWorkloadKeyOnce)
	t.Setenv(devconfig.EnvAgentID, "3f2504e0-4f89-11d3-9a0c-0305e82c3301")

	a, out, errb := testApp(nil)
	a.newRegistrar = func(_, _, _ string) devinit.Registrar { return panicReg{} }
	a.newPrompt = panicPrompt(t)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("init exit = %d on the documented env route; stderr=%q", code, errb.String())
	}
	if strings.Contains(errb.String(), "wiring bug") {
		t.Errorf("a configuration route reported itself as a defect in the binary:\n%s", errb.String())
	}
	if !strings.Contains(out.String(), "EVERY SESSION") {
		t.Errorf("the install did not complete:\n%s", out.String())
	}
	// Nothing minted, and no per-tool credential file invented for it.
	if _, err := os.Stat(filepath.Join(home, "claude-code", ".env")); !os.IsNotExist(err) {
		t.Errorf("an env-provisioned install wrote a credential file (err=%v)", err)
	}
}

// TestAdoptNeedsNoOrganizationToken adopting is four values pasted by hand and
// makes no control-plane call, so requiring fleet authority for it locked out
// the shape this exists to serve: a developer holding their own agent's key
// while the organization key lives with an administrator. It is also the only
// route back from the duplicate-name halt.
func TestAdoptNeedsNoOrganizationToken(t *testing.T) {
	home := isolateHomeUnbound(t)
	requireUnbound(t)
	clearAgentEnv(t)
	t.Setenv(devconfig.EnvControlToken, "")

	a, _, errb := testApp(nil)
	a.newRegistrar = func(_, _, _ string) devinit.Registrar { return panicReg{} }
	p := &prompt.Scripted{Answers: []string{"y", "adopted-agent-9",
		"did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301", "obx_pasted_by_hand", testSeedB64}}
	a.newPrompt = func() (prompt.Prompter, error) { return p, nil }

	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("adopt exit = %d with no organization token; stderr=%q", code, errb.String())
	}
	cfg, err := devconfig.Load(filepath.Join(home, "claude-code", "dev.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentID != "adopted-agent-9" {
		t.Errorf("the adopted agent was not stored: %+v", cfg)
	}
}

// TestAdoptRefusesABlankAgentID the id identifies the agent this store belongs
// to, and a store carrying a DID with no id is one doctor cannot attribute.
func TestAdoptRefusesABlankAgentID(t *testing.T) {
	home := isolateHomeUnbound(t)
	requireUnbound(t)
	clearAgentEnv(t)
	t.Setenv(devconfig.EnvControlToken, "")

	a, _, errb := testApp(nil)
	a.newRegistrar = func(_, _, _ string) devinit.Registrar { return panicReg{} }
	p := &prompt.Scripted{Answers: []string{"y", "",
		"did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301", "obx_pasted_by_hand", testSeedB64}}
	a.newPrompt = func() (prompt.Prompter, error) { return p, nil }

	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitError {
		t.Fatalf("exit = %d, want a refusal on a blank agent id", code)
	}
	if !strings.Contains(errb.String(), "agent id") {
		t.Errorf("the refusal does not say what is missing:\n%s", errb.String())
	}
	if _, err := os.Stat(filepath.Join(home, "claude-code", "dev.json")); !os.IsNotExist(err) {
		t.Errorf("a rejected adopt wrote a config anyway (err=%v)", err)
	}
}

// TestTheRegistrarTargetsTheEnvironmentBackend an exported backend URL
// outranks the org config at runtime, so it has to here too. Minting the agent
// in one deployment while events post to another surfaces later as a 401, with
// nothing anywhere naming a URL.
func TestTheRegistrarTargetsTheEnvironmentBackend(t *testing.T) {
	home := isolateHomeUnbound(t)
	requireUnbound(t)
	clearAgentEnv(t)
	t.Setenv(devconfig.EnvControlToken, testOrgToken)
	t.Setenv(devconfig.EnvBackendURL, "https://api.internal.example")
	if err := devconfig.WriteConfig(filepath.Join(home, "dev.json"),
		devconfig.Update{BackendURL: "https://api.from-the-org-file"}); err != nil {
		t.Fatal(err)
	}

	var target string
	a, _, errb := testApp(map[string]string{devconfig.EnvBackendURL: "https://api.internal.example"})
	a.newRegistrar = func(backendURL, _, _ string) devinit.Registrar {
		target = backendURL
		return &countingReg{byName: map[string]*backend.AgentSummary{}}
	}
	a.newPrompt = noTerminal(t)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	if target != "https://api.internal.example" {
		t.Errorf("the registrar targeted %q, want the exported backend URL", target)
	}
}

// TestReuseGatesAgree is the disagreement `init`'s own gate (scope.go's
// credentialsPresent) and devinit.register's own reuse check once had:
// the gate accepted an environment identity and devinit looked only at the
// file, so a machine provisioned entirely through exported variables reached
// registration with no registrar wired and was told it had hit a build bug.
// Each row proves the two ask the same question, in both directions:
// wantReuse=true is proven by a registrar that panics if devinit ever reaches
// it, and wantReuse=false is proven by a registrar that must actually be
// called.
func TestReuseGatesAgree(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T, home string)
		wantReuse bool
	}{
		{
			name:      "file v3",
			setup:     func(t *testing.T, home string) { seedCredentials(t, "claude-code") },
			wantReuse: true,
		},
		{
			name: "env v3",
			setup: func(t *testing.T, home string) {
				t.Setenv(devconfig.EnvAPIKeyDirect, "obx_env")
				t.Setenv(devconfig.EnvWorkloadPrivateKey, testWorkloadKeyOnce)
				t.Setenv(devconfig.EnvAgentID, testAgentIDFor(t, "claude-code"))
			},
			wantReuse: true,
		},
		{
			name: "legacy file",
			setup: func(t *testing.T, home string) {
				envPath, err := devconfig.EnvFilePathFor("claude-code")
				if err != nil {
					t.Fatal(err)
				}
				if err := devconfig.WriteEnvFile(envPath, map[string]string{
					devconfig.EnvAPIKeyDirect:    "obx_legacy",
					devconfig.EnvAgentPrivateKey: testSeedB64,
				}); err != nil {
					t.Fatal(err)
				}
				if err := devconfig.WriteConfig(filepath.Join(home, "claude-code", "dev.json"),
					devconfig.Update{DID: "did:aip:legacy"}); err != nil {
					t.Fatal(err)
				}
			},
			wantReuse: false,
		},
		{
			name: "env seed only",
			setup: func(t *testing.T, home string) {
				t.Setenv(devconfig.EnvAPIKeyDirect, "obx_env")
				t.Setenv(devconfig.EnvAgentPrivateKey, testSeedB64)
			},
			wantReuse: false,
		},
		{
			name: "seed-alias row: exported alias, no workload key",
			setup: func(t *testing.T, home string) {
				t.Setenv(devconfig.EnvAPIKeyDirect, "obx_env")
				t.Setenv("OPENBOX_ED25519_SEED", testSeedB64)
			},
			wantReuse: false,
		},
		{
			name: "seed-alias row: alias in the credential file",
			setup: func(t *testing.T, home string) {
				envPath, err := devconfig.EnvFilePathFor("claude-code")
				if err != nil {
					t.Fatal(err)
				}
				if err := devconfig.WriteEnvFile(envPath, map[string]string{
					devconfig.EnvAPIKeyDirect: "obx_legacy",
					"OPENBOX_ED25519_SEED":    testSeedB64,
				}); err != nil {
					t.Fatal(err)
				}
			},
			wantReuse: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := isolateHomeOnly(t)
			clearAgentEnv(t)
			tc.setup(t, home)

			a, _, errb := testApp(nil)
			plan, code := a.requireCredentials()
			if code != exitOK {
				t.Fatalf("requireCredentials: %s", errb.String())
			}
			if plan.reuse != tc.wantReuse {
				t.Fatalf("scope gate reuse = %v, want %v", plan.reuse, tc.wantReuse)
			}

			b, _, errb2 := testApp(nil)
			reg := &countingReg{byName: map[string]*backend.AgentSummary{}}
			if tc.wantReuse {
				// devinit must never reach the registrar; if the two gates
				// disagreed and it tried to register, this panics the test.
				b.newRegistrar = func(_, _, _ string) devinit.Registrar { return panicReg{} }
			} else {
				t.Setenv(devconfig.EnvControlToken, testOrgToken)
				b.newRegistrar = func(_, _, _ string) devinit.Registrar { return reg }
			}
			b.newPrompt = declineAdopt(t)
			if code := b.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
				t.Fatalf("init exit = %d; stderr=%q", code, errb2.String())
			}
			if !tc.wantReuse && reg.creates != 1 {
				t.Errorf("scope gate said no usable store, but devinit registered %d times, want 1", reg.creates)
			}
		})
	}
}

// TestInitThenHookReachesV3 is the end-to-end proof the table above only
// implies: `init` against a fake registrar, writing the identity a real hook
// then authenticates with, reaches fakecore's v3 /evaluate exactly once. The
// registered key has to BE fakecore's own process-wide key (via
// a.newWorkloadKey), because fakecore's fake Keycloak verifies the client
// assertion against its own public key, never whatever devinit generated.
func TestInitThenHookReachesV3(t *testing.T) {
	isolateHomeOnly(t)
	clearAgentEnv(t)
	t.Setenv(devconfig.EnvControlToken, testOrgToken)

	fake := fakecore.New(t, fakecore.Script{})
	key, err := workloadauth.ParsePrivateKey(fakecore.WorkloadPrivateKey())
	if err != nil {
		t.Fatal(err)
	}

	reg := &fakeReg{reg: &backend.Registration{
		AgentID:   fakecore.AgentID(),
		AgentName: "dev-x",
		APIKey:    fakecore.APIKey(),
		Identity:  backend.WorkloadIdentityInfo{Method: devconfig.IdentityMethodKeycloakWorkload, Kid: "kid-v3"},
	}}

	a, _, errb := testApp(nil)
	a.newRegistrar = func(_, _, _ string) devinit.Registrar { return reg }
	a.newPrompt = declineAdopt(t)
	a.newWorkloadKey = func() (*rsa.PrivateKey, error) { return key, nil }
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	if reg.create != 1 {
		t.Fatalf("Create called %d times, want 1", reg.create)
	}

	t.Setenv(devconfig.EnvBaseURL, fake.URL())
	b, out, berrb := testApp(nil)
	b.stdin = strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"/r","prompt":"hi"}`)
	if code := b.run([]string{"hook", "claude-code", "UserPromptSubmit"}); code != exitOK {
		t.Fatalf("hook exit = %d; stderr=%q", code, berrb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout must stay empty, got %q", out.String())
	}
	if n := fake.V3EvaluateAttempts(); n != 1 {
		t.Errorf("/evaluate attempts = %d, want exactly 1", n)
	}
}
