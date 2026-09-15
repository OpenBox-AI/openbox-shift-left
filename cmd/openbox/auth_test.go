package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/devinit"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/prompt"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

var testSeedB64 = base64.StdEncoding.EncodeToString(make([]byte, 32))

func readEnvFile(t *testing.T, home string) map[string]string {
	t.Helper()
	kv, err := devconfig.ParseEnvFile(filepath.Join(home, ".env"))
	if err != nil {
		t.Fatalf("parse credential file: %v", err)
	}
	return kv
}

func readDevJSON(t *testing.T, home string) devconfig.DevConfig {
	t.Helper()
	cfg, err := devconfig.Load(filepath.Join(home, "dev.json"))
	if err != nil {
		t.Fatalf("load dev.json: %v", err)
	}
	return cfg
}

// TestAuthPromptsTheOrgConnectionOnly `auth` is the org connection and
// nothing else: two URLs and the token every tool shares. The agent id, DID,
// API key and signing key prompts are gone with the registration they fed --
// `init --provider <tool>` mints that tool's agent now -- so a script holding
// exactly three answers must be exhausted, and a fourth prompt would fail the
// run by running out of answers.
func TestAuthPromptsTheOrgConnectionOnly(t *testing.T) {
	home := isolateHome(t)
	a, _, errb := testApp(nil)
	p := scriptedAuth(t, a, "", "", "obx_key_"+strings.Repeat("f", 48))
	// Registration is not merely unused here: it must be unreachable.
	a.newRegistrar = func(_, _, _ string) devinit.Registrar {
		panic("auth registered an agent; init owns registration now")
	}

	if code := a.run([]string{"auth"}); code != exitOK {
		t.Fatalf("exit = %d; stderr=%q", code, errb.String())
	}
	if p.Remaining() != 0 {
		t.Errorf("Remaining = %d; auth asked fewer than its three questions", p.Remaining())
	}
	want := []string{"Backend URL (control plane)", "Core URL (data plane)",
		"Organization control token (obx_key_… or JWT)"}
	if len(p.Prompts) != len(want) {
		t.Fatalf("prompts = %v, want %v", p.Prompts, want)
	}
	for i := range want {
		if p.Prompts[i] != want[i] {
			t.Errorf("prompt[%d] = %q, want %q", i, p.Prompts[i], want[i])
		}
	}
	// Not one per-tool file: identity is init's to write, per tool.
	for _, tool := range provider.Supported() {
		if _, err := os.Stat(filepath.Join(home, tool)); !os.IsNotExist(err) {
			t.Errorf("auth created %s's identity directory (err=%v)", tool, err)
		}
	}
}

// TestTheTokenPromptNeverEchoesAStoredValue the stored token prefills as
// "there is one" and never as a value: Secret does not display what it holds,
// which is the whole reason the token is asked for through it.
func TestTheTokenPromptNeverEchoesAStoredValue(t *testing.T) {
	isolateHome(t)
	stored := "obx_key_" + strings.Repeat("e", 48)
	orgEnv, err := devconfig.OrgEnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteEnvFile(orgEnv, map[string]string{devconfig.EnvControlToken: stored}); err != nil {
		t.Fatal(err)
	}

	a, _, errb := testApp(nil)
	p := scriptedAuth(t, a, "", "", "")
	if code := a.run([]string{"auth"}); code != exitOK {
		t.Fatalf("exit = %d; stderr=%q", code, errb.String())
	}
	if strings.Contains(p.Out.String(), stored) {
		t.Errorf("the stored token was offered as a prompt default:\n%s", p.Out.String())
	}
	// And a blank answer kept it, which is what makes a re-run to fix one URL safe.
	kv, err := devconfig.ParseEnvFile(orgEnv)
	if err != nil {
		t.Fatal(err)
	}
	if kv[devconfig.EnvControlToken] != stored {
		t.Error("a blank answer erased the stored token")
	}
}

// TestURLPromptsPrefillTheHostedDefaults both URL prompts prefill with the
// hosted defaults, and accepting them writes those values rather than an empty
// string.
func TestURLPromptsPrefillTheHostedDefaults(t *testing.T) {
	p := &prompt.Scripted{Answers: []string{"", "", ""}}
	got, err := collectAuthFields(p, authFields{
		backendURL: devconfig.DefaultBackendURL,
		baseURL:    devconfig.DefaultBaseURL,
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got.backendURL != devconfig.DefaultBackendURL {
		t.Errorf("backend URL = %q, want the hosted default", got.backendURL)
	}
	if got.baseURL != devconfig.DefaultBaseURL {
		t.Errorf("core URL = %q, want the hosted default", got.baseURL)
	}
	if !strings.Contains(p.Out.String(), devconfig.DefaultBackendURL) {
		t.Errorf("the backend prompt should show the default it will accept:\n%s", p.Out.String())
	}
}

// TestURLPromptsAcceptOverrides a self-hosted user must be able to override
// either URL.
func TestURLPromptsAcceptOverrides(t *testing.T) {
	p := &prompt.Scripted{Answers: []string{"https://api.internal", "https://core.internal", ""}}
	got, err := collectAuthFields(p, authFields{backendURL: devconfig.DefaultBackendURL, baseURL: devconfig.DefaultBaseURL})
	if err != nil {
		t.Fatal(err)
	}
	if got.backendURL != "https://api.internal" || got.baseURL != "https://core.internal" {
		t.Errorf("overrides not honoured: %+v", got)
	}
}

// TestBlankKeepsCurrentValues blank input keeps the current value, which is
// what makes a re-run safe: pressing Enter through every field must not erase
// the org token.
func TestBlankKeepsCurrentValues(t *testing.T) {
	current := authFields{backendURL: "https://api.internal", baseURL: "https://core.internal"}
	current.controlToken = "obx_key_" + strings.Repeat("f", 48)
	p := &prompt.Scripted{Answers: []string{"", "", ""}}
	got, err := collectAuthFields(p, current)
	if err != nil {
		t.Fatal(err)
	}
	if got != current {
		t.Errorf("blank input changed values:\n got %+v\nwant %+v", got, current)
	}
}

// TestAuthNeverTouchesPosture tHE posture guard. `auth` must build
// devconfig.Update literally, leaving every posture pointer nil, so
// WriteConfig's tri-state merge carries the developer's posture forward
// untouched.
func TestAuthNeverTouchesPosture(t *testing.T) {
	home := isolateHome(t)
	devPath := filepath.Join(home, "dev.json")

	tr, fa := true, false
	if err := devconfig.WriteConfig(devPath, devconfig.Update{
		DID:     "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301",
		Enforce: &tr, Tier2: &tr, Findings: &tr,
		ContentCapture: &fa, InstallGitHook: &tr,
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(devPath)
	if err != nil {
		t.Fatal(err)
	}
	var postureBefore map[string]any
	if err := json.Unmarshal(before, &postureBefore); err != nil {
		t.Fatal(err)
	}

	a, _, _ := testApp(nil)
	if code := a.writeCoordinates(authFields{
		backendURL: "https://api.internal", baseURL: "https://core.internal",
	}); code != exitOK {
		t.Fatalf("writeCoordinates exit = %d", code)
	}

	var postureAfter map[string]any
	raw, _ := os.ReadFile(devPath)
	if err := json.Unmarshal(raw, &postureAfter); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"enforce", "tier2", "findings", "content_capture", "install_git_hook"} {
		if postureBefore[field] != postureAfter[field] {
			t.Errorf("posture field %q changed: %v → %v; auth must not write posture",
				field, postureBefore[field], postureAfter[field])
		}
	}
	cfg := readDevJSON(t, home)
	if cfg.BackendURL != "https://api.internal" || cfg.BaseURL != "https://core.internal" {
		t.Errorf("coordinates not written: %+v", cfg)
	}
	// The agent id came off this write with registration: it identifies one
	// tool's agent, and the org config is shared by all of them.
	if cfg.AgentID != "" {
		t.Errorf("auth wrote an agent id (%q) into the org config", cfg.AgentID)
	}
}

// TestAuthDoesNotTripTheEnforceDowngradeGuard wouldDowngradeEnforce must not
// fire for an auth run: auth never proposes an enforce change, so there is no
// posture change to announce.
func TestAuthDoesNotTripTheEnforceDowngradeGuard(t *testing.T) {
	home := isolateHome(t)
	devPath := filepath.Join(home, "dev.json")
	tr := true
	if err := devconfig.WriteConfig(devPath, devconfig.Update{DID: "did:aip:x", Enforce: &tr}); err != nil {
		t.Fatal(err)
	}
	if devconfig.WouldDowngradeEnforce(devPath, nil) {
		t.Error("a nil Enforce must never register as a downgrade")
	}
}

// TestSecretsAndCoordinatesGoToDifferentFiles tHE split, with both halves
// replaced: the secret is the org control token now and the coordinates are
// the two URLs. The rule survived the replacement and is what it always was --
// a coordinate in .env or a secret in dev.json reintroduces the stale-copy bug
// that reverted a corrected DID on every install.
func TestSecretsAndCoordinatesGoToDifferentFiles(t *testing.T) {
	home := isolateHome(t)
	a, _, _ := testApp(nil)
	token := "obx_key_" + strings.Repeat("f", 48)
	f := authFields{backendURL: "https://api.internal", baseURL: "https://core.internal"}
	f.controlToken = token

	orgEnv, err := devconfig.OrgEnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if code := a.writeSecrets(orgEnv, f); code != exitOK {
		t.Fatalf("writeSecrets exit = %d", code)
	}
	if code := a.writeCoordinates(f); code != exitOK {
		t.Fatalf("writeCoordinates exit = %d", code)
	}

	envRaw, err := os.ReadFile(orgEnv)
	if err != nil {
		t.Fatal(err)
	}
	devRaw, err := os.ReadFile(filepath.Join(home, "dev.json"))
	if err != nil {
		t.Fatal(err)
	}

	for _, coord := range []string{devconfig.EnvDID, devconfig.EnvAgentID, devconfig.EnvBaseURL, devconfig.EnvBackendURL} {
		if strings.Contains(string(envRaw), coord+"=") {
			t.Errorf(".env carries the coordinate %s; secrets and coordinates must not share a file:\n%s", coord, envRaw)
		}
	}
	if strings.Contains(string(devRaw), token) {
		t.Errorf("dev.json leaked the control token:\n%s", devRaw)
	}
	if !strings.Contains(string(devRaw), "https://api.internal") || !strings.Contains(string(devRaw), "https://core.internal") {
		t.Errorf("the URLs were not written:\n%s", devRaw)
	}
	kv := readEnvFile(t, home)
	if kv[devconfig.EnvControlToken] != token {
		t.Errorf("the control token was not written: %v", kv)
	}
}

// TestSecondRunOverwritesTheFirst `init` structurally could not update
// credentials; the reuse path returned before any write. Auth must overwrite
// unconditionally, so a second run with different input leaves the second
// value on disk -- and a second run answering blank to everything must leave
// the files byte-identical, which is the other half of the same contract and
// the one a "keeps current" bug hides in.
func TestSecondRunOverwritesTheFirst(t *testing.T) {
	home := isolateHome(t)
	orgEnv, err := devconfig.OrgEnvFilePath()
	if err != nil {
		t.Fatal(err)
	}

	runAuthWith := func(answers ...string) {
		t.Helper()
		a, _, errb := testApp(nil)
		scriptedAuth(t, a, answers...)
		if code := a.run([]string{"auth"}); code != exitOK {
			t.Fatalf("auth exit = %d; stderr=%q", code, errb.String())
		}
	}

	runAuthWith("https://api.one", "", "obx_key_"+strings.Repeat("1", 48))
	runAuthWith("https://api.two", "", "obx_key_"+strings.Repeat("2", 48))
	kv := readEnvFile(t, home)
	if kv[devconfig.EnvControlToken] != "obx_key_"+strings.Repeat("2", 48) {
		t.Error("the control token is not the second value")
	}
	cfg := readDevJSON(t, home)
	if cfg.BackendURL != "https://api.two" {
		t.Errorf("backend URL = %q, want the second value", cfg.BackendURL)
	}

	envBefore, err := os.ReadFile(orgEnv)
	if err != nil {
		t.Fatal(err)
	}
	devBefore, err := os.ReadFile(filepath.Join(home, "dev.json"))
	if err != nil {
		t.Fatal(err)
	}
	runAuthWith("", "", "")
	envAfter, err := os.ReadFile(orgEnv)
	if err != nil {
		t.Fatal(err)
	}
	devAfter, err := os.ReadFile(filepath.Join(home, "dev.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(envBefore) != string(envAfter) {
		t.Errorf("an all-blank re-run changed the credential file:\n%s\n---\n%s", envBefore, envAfter)
	}
	if string(devBefore) != string(devAfter) {
		t.Errorf("an all-blank re-run changed the dev config:\n%s\n---\n%s", devBefore, devAfter)
	}
}

// TestTheControlTokenIsPersistedOrgLevelOnly inverts what this file used to
// assert. The token WAS never persisted, because the only thing that read it
// from disk was the approver persona and the file is plaintext. `auth` and
// `init` are now two processes with nothing exported between them, so it has
// to survive on disk -- and the exposure that argued against it is unchanged
// and accepted (owner ruling O1).
//
// What is not negotiable is which file. This credential creates and rotates
// agents across the whole organization; keeping it out of every per-tool store
// is what stops one compromised tool store from being a fleet compromise, and
// the negative half of this case is the enforcement of that, not the comment.
func TestTheControlTokenIsPersistedOrgLevelOnly(t *testing.T) {
	home := isolateHome(t)
	token := "obx_key_" + strings.Repeat("f", 48)

	a, _, errb := testApp(nil)
	scriptedAuth(t, a, "", "", token)
	if code := a.run([]string{"auth"}); code != exitOK {
		t.Fatalf("auth exit = %d; stderr=%q", code, errb.String())
	}

	kv := readEnvFile(t, home)
	if kv[devconfig.EnvControlToken] != token {
		t.Errorf("the org control token was not persisted to the org credential file: %v", kv)
	}
	for _, agentKey := range []string{devconfig.EnvAPIKeyDirect, devconfig.EnvAgentPrivateKey} {
		if _, ok := kv[agentKey]; ok {
			t.Errorf("auth wrote %s; agent credentials are init's, per tool", agentKey)
		}
	}
	// And nowhere else. Not a per-tool store, and not the dev config.
	for _, tool := range provider.Supported() {
		perTool, err := devconfig.EnvFilePathFor(tool)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(perTool); !os.IsNotExist(err) {
			t.Errorf("auth created %s (err=%v); the control token must never reach a per-tool store", perTool, err)
		}
	}
	devRaw, err := os.ReadFile(filepath.Join(home, "dev.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(devRaw), token) {
		t.Errorf("the control token reached dev.json:\n%s", devRaw)
	}
	// It is resolvable from the file by a later process, which is the whole
	// point of persisting it.
	t.Setenv(devconfig.EnvControlToken, "")
	if got := devconfig.ResolveControlToken(); got != token {
		t.Error("the persisted token does not resolve back from the org file")
	}
}

// TestEnvShadowWarningNamesTheRightFile a real env var beats both files, so
// writing while one is exported produces a config that silently has no effect.
// Three pairs, because auth writes three things now: warning about a variable
// that shadows a file this run did not touch sends somebody unsetting a
// variable that is doing no harm.
func TestEnvShadowWarningNamesTheRightFile(t *testing.T) {
	home := isolateHome(t)
	envPath := filepath.Join(home, ".env")
	devPath := filepath.Join(home, "dev.json")

	for _, tc := range []struct{ varName, wantFile string }{
		{devconfig.EnvBaseURL, devPath},
		{devconfig.EnvBackendURL, devPath},
	} {
		t.Run(tc.varName, func(t *testing.T) {
			a, _, errb := testApp(map[string]string{tc.varName: "set"})
			a.warnShadowedByEnv(envPath)
			s := errb.String()
			if !strings.Contains(s, tc.varName) {
				t.Errorf("warning should name %s:\n%s", tc.varName, s)
			}
			if !strings.Contains(s, tc.wantFile) {
				t.Errorf("warning for %s should name %s:\n%s", tc.varName, tc.wantFile, s)
			}
		})
	}
	// The control token inverted with its precedence: the file wins now, so the
	// warning must say the export is being ignored rather than that it wins.
	// A warning pointing the wrong way is worse than none -- it sends its reader
	// to unset a variable that is already inert, and leaves the real cause of a
	// later refusal unexplained.
	t.Run(devconfig.EnvControlToken, func(t *testing.T) {
		a, _, errb := testApp(map[string]string{devconfig.EnvControlToken: "set"})
		a.warnShadowedByEnv(envPath)
		s := errb.String()
		if !strings.Contains(s, devconfig.EnvControlToken) || !strings.Contains(s, envPath) {
			t.Errorf("warning does not name the variable and the file:\n%s", s)
		}
		if !strings.Contains(s, "IGNORED") {
			t.Errorf("warning does not say the export is ignored:\n%s", s)
		}
		if strings.Contains(s, "so it overrides what was just written") {
			t.Errorf("warning still claims the environment wins:\n%s", s)
		}
	})

	// And not about anything auth no longer writes.
	for _, gone := range []string{devconfig.EnvAPIKeyDirect, devconfig.EnvAgentPrivateKey, devconfig.EnvDID, devconfig.EnvAgentID} {
		a, _, errb := testApp(map[string]string{gone: "set"})
		a.warnShadowedByEnv(envPath)
		if strings.Contains(errb.String(), gone) {
			t.Errorf("auth warned that %s shadows a file it does not write:\n%s", gone, errb.String())
		}
	}
}

// TestEnvShadowStillWrites warn, never refuse: exporting these in CI is a
// documented pattern.
func TestEnvShadowStillWrites(t *testing.T) {
	home := isolateHome(t)
	a, _, _ := testApp(map[string]string{devconfig.EnvControlToken: "obx_key_from_env"})
	orgEnv, err := devconfig.OrgEnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	f := authFields{}
	f.controlToken = "obx_key_written"
	if code := a.writeSecrets(orgEnv, f); code != exitOK {
		t.Fatal("a shadowed field must still be written")
	}
	if readEnvFile(t, home)[devconfig.EnvControlToken] != "obx_key_written" {
		t.Error("the file should hold what auth wrote, regardless of the environment")
	}
}

func TestAuthIsDispatchedAndInHelp(t *testing.T) {
	a, _, errb := testApp(nil)
	a.usage()
	s := errb.String()
	if !strings.Contains(s, "openbox auth") {
		t.Errorf("usage should list auth:\n%s", s)
	}
	if strings.Index(s, "openbox auth") > strings.Index(s, "openbox init --provider") {
		t.Errorf("auth should be listed before init:\n%s", s)
	}
}

// TestNoAuthFlagTakesASecretValue no flag may accept a secret value (INV-1):
// flags name sources.
func TestNoAuthFlagTakesASecretValue(t *testing.T) {
	raw, err := os.ReadFile("auth.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, banned := range []string{
		`StringVar(&apiKey, "api-key"`,
		`StringVar(&privateKey, "private-key"`,
		`"api-key",`,
		`"private-key",`,
		`"control-token",`,
	} {
		if strings.Contains(body, banned) {
			t.Errorf("auth.go defines a flag that takes a secret value: %s", banned)
		}
	}
	// The stronger claim now, and the one that subsumes the list above: `auth`
	// registers no flags at all, so there is no flag left that could take a
	// secret value.
	if strings.Contains(body, "fs.StringVar(") || strings.Contains(body, "fs.BoolVar(") {
		t.Error("auth.go registers a flag; `openbox auth` takes none")
	}
}

// TestNonInteractiveFailsFastAndNamesTheProvisioningRoutes. A command that
// blocks on stdin in CI hangs to the job timeout with no output, so this has to
// fail immediately — and the failure has to say what to do instead, because
// there is no flag left to reach for.
func TestNonInteractiveFailsFastAndNamesTheProvisioningRoutes(t *testing.T) {
	isolateHome(t)
	a, _, errb := testApp(nil)
	// The real terminal gate, not the test seam: this is what a pipe hits.
	a.newPrompt = func() (prompt.Prompter, error) {
		return nil, fmt.Errorf("%w\n%s", prompt.ErrNotATerminal, prompt.NonInteractiveHelp)
	}
	code := a.run([]string{"auth"})
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	for _, want := range []string{"OPENBOX_API_KEY", ".openbox/.env", "dev.json"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("the failure does not name %q as a provisioning route:\n%s", want, errb.String())
		}
	}
}

// TestAuthSuccessNamesInitAsTheNextStep success names the command that
// actually installs governance: auth alone governs nothing, and a user who
// stops here has telemetry from no session at all.
// TestAuthTakesNoFlagAtAll. Thirteen of them are gone, and each was also a way
// to get this wrong: a secret in argv, a confirmation skipped, a credential file
// written where the hooks do not read. A flag that parses and does nothing would
// be worse than one that fails.
func TestAuthTakesNoFlagAtAll(t *testing.T) {
	for _, flag := range []string{
		"--rotate", "--yes", "--api-key-stdin", "--private-key-stdin", "--env-file",
		"--icon", "--description", "--force", "--base-url", "--backend-url",
		"--did", "--agent-id", "--control-token-stdin",
	} {
		a, _, errb := testApp(nil)
		if code := a.run([]string{"auth", flag, "x"}); code == exitOK {
			t.Errorf("auth accepted %s", flag)
			continue
		}
		if !strings.Contains(errb.String(), "not defined") {
			t.Errorf("%s was refused for the wrong reason: %s", flag, errb.String())
		}
	}
}

func TestAuthSuccessNamesInitAsTheNextStep(t *testing.T) {
	isolateHome(t)
	a, out, errb := testApp(nil)
	scriptedAuth(t, a, "", "", "obx_key_"+strings.Repeat("f", 48))
	if code := a.run([]string{"auth"}); code != exitOK {
		t.Fatalf("exit = %d; stderr=%q", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "openbox init") {
		t.Errorf("success output should name `openbox init`:\n%s", s)
	}
	// Each tool carries its own agent now, so the next step is per tool and the
	// output has to say so or a second tool is silently ungoverned.
	if !strings.Contains(s, "once per tool") {
		t.Errorf("success output should say init is run per tool:\n%s", s)
	}
	// One install governs the whole machine, so telling somebody to repeat it per
	// project would send them doing work that does nothing.
	if !strings.Contains(s, "every session on this machine") {
		t.Errorf("success output should state what the next step governs:\n%s", s)
	}
	for _, gone := range []string{"THIS DIRECTORY", "--scope", "each project"} {
		if strings.Contains(s, gone) {
			t.Errorf("success output still describes per-project scope (%q):\n%s", gone, s)
		}
	}
}
