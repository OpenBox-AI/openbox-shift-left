package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/backend"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/devinit"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/prompt"
)

// A real 32-byte Ed25519 seed, base64: what OpenBox actually hands out.
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

// --- collectAuthFields: the prompt order and the short-circuit ---------------

// The DX target: a first run with an org key exported completes with three
// Enters and one y. The DID, api key and signing key must never be shown on the
// register path — registration returns all three, so asking for them first is
// pure friction.
func TestBlankAgentIDShortCircuitsTheCredentialPrompts(t *testing.T) {
	// backend URL, core URL, agent id — then nothing.
	p := &prompt.Scripted{Answers: []string{"", "", "", "SHOULD-NOT-BE-READ", "SHOULD-NOT-BE-READ"}}
	got, err := collectAuthFields(p, authFields{
		backendURL: devconfig.DefaultBackendURL,
		baseURL:    devconfig.DefaultBaseURL,
	}, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !got.register {
		t.Fatal("a blank agent id must set register")
	}
	if got.did != "" || got.apiKey != "" || got.privateKey != "" {
		t.Errorf("register path read credentials it should have skipped: %+v", got)
	}
	if p.Remaining() != 2 {
		t.Errorf("Remaining = %d, want 2 — the DID and secret prompts must not be shown", p.Remaining())
	}
	// The prompt ORDER is the UX contract.
	want := []string{"Backend URL (control plane)", "Core URL (data plane)", "Agent id (blank registers a new agent)"}
	if len(p.Prompts) != len(want) {
		t.Fatalf("prompts = %v, want %v", p.Prompts, want)
	}
	for i := range want {
		if p.Prompts[i] != want[i] {
			t.Errorf("prompt[%d] = %q, want %q", i, p.Prompts[i], want[i])
		}
	}
}

// Both URL prompts prefill with the hosted defaults, and accepting them writes
// those values rather than an empty string.
func TestURLPromptsPrefillTheHostedDefaults(t *testing.T) {
	p := &prompt.Scripted{Answers: []string{"", "", "agent-1", "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301", "obx_k", testSeedB64}}
	got, err := collectAuthFields(p, authFields{
		backendURL: devconfig.DefaultBackendURL,
		baseURL:    devconfig.DefaultBaseURL,
	}, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got.backendURL != devconfig.DefaultBackendURL {
		t.Errorf("backend URL = %q, want the hosted default", got.backendURL)
	}
	if got.baseURL != devconfig.DefaultBaseURL {
		t.Errorf("core URL = %q, want the hosted default", got.baseURL)
	}
	if got.register {
		t.Error("an agent id was given; register must stay false")
	}
	if !strings.Contains(p.Out.String(), devconfig.DefaultBackendURL) {
		t.Errorf("the backend prompt should show the default it will accept:\n%s", p.Out.String())
	}
}

// A self-hosted user must be able to override either URL.
func TestURLPromptsAcceptOverrides(t *testing.T) {
	p := &prompt.Scripted{Answers: []string{"https://api.internal", "https://core.internal", "agent-1", "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301", "obx_k", testSeedB64}}
	got, err := collectAuthFields(p, authFields{backendURL: devconfig.DefaultBackendURL, baseURL: devconfig.DefaultBaseURL}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.backendURL != "https://api.internal" || got.baseURL != "https://core.internal" {
		t.Errorf("overrides not honoured: %+v", got)
	}
}

// Blank input keeps the current value, which is what makes a re-run safe:
// pressing Enter through every field must not erase a credential.
//
// The agent id is the ONE exception and is typed here rather than left blank,
// because blank there means "register a new agent" — see
// TestAgentIDPromptNeverPrefills. Everything else, including both secrets, must
// survive an all-Enter re-run.
func TestBlankKeepsCurrentValues(t *testing.T) {
	current := authFields{backendURL: "https://api.internal", baseURL: "https://core.internal",
		agentID: "agent-1", did: "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301",
		apiKey: "obx_existing", privateKey: testSeedB64,
	}
	p := &prompt.Scripted{Answers: []string{"", "", "agent-1", "", "", ""}}
	got, err := collectAuthFields(p, current, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != current {
		t.Errorf("blank input changed values:\n got %+v\nwant %+v", got, current)
	}
}

// --- validation --------------------------------------------------------------

// The org/agent key mix-up cost hours of debugging once. Pasting an obx_key_ into
// the api-key field must be rejected by name.
func TestOrgKeyInTheAPIKeyFieldIsRejected(t *testing.T) {
	problem := validateAuthFields(authFields{
		apiKey: "obx_key_" + strings.Repeat("f", 48), privateKey: testSeedB64,
		did: "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301",
	})
	if problem == "" {
		t.Fatal("an obx_key_ org key in the api-key field must be rejected")
	}
	for _, want := range []string{"ORGANIZATION key", devconfig.EnvControlToken, "obx_"} {
		if !strings.Contains(problem, want) {
			t.Errorf("rejection should mention %q:\n%s", want, problem)
		}
	}
	// It must not echo the whole credential back.
	if strings.Contains(problem, strings.Repeat("f", 48)) {
		t.Errorf("rejection echoed the credential body:\n%s", problem)
	}
}

func TestPrivateKeyValidation(t *testing.T) {
	for _, tc := range []struct {
		name, key, wantText string
	}{
		{name: "valid 32-byte seed", key: testSeedB64, wantText: ""},
		{name: "not base64", key: "!!!not base64!!!", wantText: "not valid base64"},
		{name: "wrong length", key: base64.StdEncoding.EncodeToString([]byte("short")), wantText: "decodes to 5 bytes"},
		{name: "empty", key: "", wantText: "no signing key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := privateKeyProblem(tc.key)
			if tc.wantText == "" {
				if got != "" {
					t.Fatalf("valid key rejected: %s", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantText) {
				t.Errorf("problem = %q, want it to contain %q", got, tc.wantText)
			}
			// A private key must never be echoed, not even a malformed one.
			if tc.key != "" && strings.Contains(got, tc.key) {
				t.Errorf("validation echoed the key: %s", got)
			}
		})
	}
}

func TestDIDShapeValidation(t *testing.T) {
	base := authFields{apiKey: "obx_k", privateKey: testSeedB64}
	for _, tc := range []struct {
		did      string
		wantFail bool
	}{
		{"did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301", false},
		{"did:aip:not-a-uuid", true},
		{"did:web:example.com", true},
		{"3f2504e0-4f89-11d3-9a0c-0305e82c3301", true},
		{"", true},
	} {
		f := base
		f.did = tc.did
		problem := validateAuthFields(f)
		if tc.wantFail && problem == "" {
			t.Errorf("DID %q should have been rejected", tc.did)
		}
		if !tc.wantFail && problem != "" {
			t.Errorf("DID %q rejected: %s", tc.did, problem)
		}
	}
}

// Nothing is validated on the register path: the server supplies all of it.
func TestRegisterPathSkipsValidation(t *testing.T) {
	if problem := validateAuthFields(authFields{register: true}); problem != "" {
		t.Errorf("register path should need no local values, got: %s", problem)
	}
}

// --- masking -----------------------------------------------------------------

func TestSummaryMasksSecrets(t *testing.T) {
	home := isolateHome(t)
	a, out, _ := testApp(nil)
	const apiKey = "obx_live_SENSITIVEBODY_a91f"
	a.printAuthSummary(authFields{
		apiKey: apiKey, privateKey: testSeedB64,
		agentID: "agent-1", did: "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301",
		backendURL: devconfig.DefaultBackendURL, baseURL: devconfig.DefaultBaseURL,
	}, filepath.Join(home, ".env"))

	s := out.String()
	if strings.Contains(s, "SENSITIVEBODY") {
		t.Errorf("summary printed the api key body:\n%s", s)
	}
	if strings.Contains(s, testSeedB64) {
		t.Errorf("summary printed the signing key:\n%s", s)
	}
	// Enough to recognise WHICH key it is: prefix, last 4, length.
	if !strings.Contains(s, "a91f") || !strings.Contains(s, "chars)") {
		t.Errorf("summary should show a recognizable masked token:\n%s", s)
	}
	// Coordinates are not secret and appear in full.
	if !strings.Contains(s, "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301") || !strings.Contains(s, "agent-1") {
		t.Errorf("summary should show the coordinates in full:\n%s", s)
	}
}

// The fingerprint is of the DERIVED PUBLIC key. Fingerprinting the seed would
// publish a hash of private key material.
func TestFingerprintIsOfThePublicKeyNotTheSeed(t *testing.T) {
	fp := publicKeyFingerprint(testSeedB64)
	if !strings.HasPrefix(fp, "SHA256:") || !strings.Contains(fp, "public key") {
		t.Errorf("fingerprint = %q", fp)
	}
	if strings.Contains(fp, testSeedB64) {
		t.Errorf("fingerprint contains the seed: %q", fp)
	}
	// Two different seeds must fingerprint differently, or the value is useless
	// for telling one identity from another.
	other := make([]byte, 32)
	other[0] = 1
	if publicKeyFingerprint(base64.StdEncoding.EncodeToString(other)) == fp {
		t.Error("two different seeds produced the same fingerprint")
	}
	if got := publicKeyFingerprint(""); got != "(none)" {
		t.Errorf("empty seed = %q, want (none)", got)
	}
}

// --- the write path ----------------------------------------------------------

// THE POSTURE GUARD. `auth` must build devconfig.Update literally, leaving every
// posture pointer nil, so WriteConfig's tri-state merge carries the developer's
// posture forward untouched. Routing through provider.ConfigUpdate by habit would
// always set InstallGitHook and silently rewrite posture.
func TestAuthNeverTouchesPosture(t *testing.T) {
	home := isolateHome(t)
	devPath := filepath.Join(home, "dev.json")

	// A developer with a deliberate posture.
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
		did:     "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301",
		agentID: "agent-1", backendURL: "https://api.internal", baseURL: "https://core.internal",
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
			t.Errorf("posture field %q changed: %v → %v — auth must not write posture",
				field, postureBefore[field], postureAfter[field])
		}
	}
	// And the coordinates it DID own are written.
	cfg := readDevJSON(t, home)
	if cfg.AgentID != "agent-1" || cfg.BackendURL != "https://api.internal" || cfg.BaseURL != "https://core.internal" {
		t.Errorf("coordinates not written: %+v", cfg)
	}
}

// WouldDowngradeEnforce must not fire for an auth run: auth never proposes an
// enforce change, so there is no posture change to announce.
func TestAuthDoesNotTripTheEnforceDowngradeGuard(t *testing.T) {
	home := isolateHome(t)
	devPath := filepath.Join(home, "dev.json")
	tr := true
	if err := devconfig.WriteConfig(devPath, devconfig.Update{DID: "did:aip:x", Enforce: &tr}); err != nil {
		t.Fatal(err)
	}
	// nil Enforce is what auth always passes.
	if devconfig.WouldDowngradeEnforce(devPath, nil) {
		t.Error("a nil Enforce must never register as a downgrade")
	}
}

// THE SPLIT. Secrets to .env, coordinates to dev.json, and NOTHING in both.
func TestSecretsAndCoordinatesGoToDifferentFiles(t *testing.T) {
	home := isolateHome(t)
	a, _, _ := testApp(nil)
	f := authFields{
		apiKey: "obx_live_key", privateKey: testSeedB64,
		did: "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301", agentID: "agent-1",
		backendURL: "https://api.internal", baseURL: "https://core.internal",
	}
	if code := a.writeSecrets(filepath.Join(home, ".env"), f, nil); code != exitOK {
		t.Fatalf("writeSecrets exit = %d", code)
	}
	if code := a.writeCoordinates(f); code != exitOK {
		t.Fatalf("writeCoordinates exit = %d", code)
	}

	envRaw, err := os.ReadFile(filepath.Join(home, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	devRaw, err := os.ReadFile(filepath.Join(home, "dev.json"))
	if err != nil {
		t.Fatal(err)
	}

	// No coordinate key in.env — the tripwire for the two-store bug that
	// decision removed. Relaxing this reopens it.
	for _, coord := range []string{devconfig.EnvDID, devconfig.EnvAgentID, devconfig.EnvBaseURL, devconfig.EnvBackendURL} {
		if strings.Contains(string(envRaw), coord+"=") {
			t.Errorf(".env carries the coordinate %s; secrets and coordinates must not share a file:\n%s", coord, envRaw)
		}
	}
	// No secret VALUE in dev.json.
	for _, secret := range []string{"obx_live_key", testSeedB64} {
		if strings.Contains(string(devRaw), secret) {
			t.Errorf("dev.json leaked a secret value:\n%s", devRaw)
		}
	}
	kv := readEnvFile(t, home)
	if kv[devconfig.EnvAPIKeyDirect] != "obx_live_key" || kv[devconfig.EnvAgentPrivateKey] != testSeedB64 {
		t.Errorf("secrets not written: %v", kv)
	}
}

// `init` structurally could not update credentials — the reuse path returned
// before any write. auth must overwrite unconditionally, so a second run with
// different input leaves the SECOND value on disk.
func TestSecondRunOverwritesTheFirst(t *testing.T) {
	home := isolateHome(t)
	a, _, _ := testApp(nil)
	envPath := filepath.Join(home, ".env")
	first := authFields{apiKey: "obx_first", privateKey: testSeedB64}
	if code := a.writeSecrets(envPath, first, nil); code != exitOK {
		t.Fatal("first write failed")
	}
	other := make([]byte, 32)
	other[0] = 9
	second := authFields{apiKey: "obx_second", privateKey: base64.StdEncoding.EncodeToString(other)}
	if code := a.writeSecrets(envPath, second, nil); code != exitOK {
		t.Fatal("second write failed")
	}
	kv := readEnvFile(t, home)
	if kv[devconfig.EnvAPIKeyDirect] != "obx_second" {
		t.Errorf("api key = %q, want the second value", kv[devconfig.EnvAPIKeyDirect])
	}
	if kv[devconfig.EnvAgentPrivateKey] != second.privateKey {
		t.Errorf("signing key = %q, want the second value", kv[devconfig.EnvAgentPrivateKey])
	}
}

// The org control token is a much larger exposure than the agent seed, so it is
// only written when explicitly supplied — never as a side effect.
func TestControlTokenWrittenOnlyWhenSupplied(t *testing.T) {
	home := isolateHome(t)
	a, _, _ := testApp(nil)
	envPath := filepath.Join(home, ".env")
	f := authFields{apiKey: "obx_k", privateKey: testSeedB64}

	if code := a.writeSecrets(envPath, f, nil); code != exitOK {
		t.Fatal("write failed")
	}
	if _, ok := readEnvFile(t, home)[devconfig.EnvControlToken]; ok {
		t.Error("an ordinary auth run must not persist an org control token")
	}

	orgKey := "obx_key_" + strings.Repeat("f", 48)
	if code := a.writeSecrets(envPath, f, map[string]string{devconfig.EnvControlToken: orgKey}); code != exitOK {
		t.Fatal("write failed")
	}
	if readEnvFile(t, home)[devconfig.EnvControlToken] != orgKey {
		t.Error("an explicitly supplied control token should be persisted")
	}
}

// --- env shadowing -----------------------------------------------------------

// A real env var beats both files, so writing while one is exported produces a
// config that silently has no effect. Warn loudly, naming the RIGHT file per field.
func TestEnvShadowWarningNamesTheRightFile(t *testing.T) {
	home := isolateHome(t)
	envPath := filepath.Join(home, ".env")
	devPath := filepath.Join(home, "dev.json")

	for _, tc := range []struct{ varName, wantFile string }{
		{devconfig.EnvAPIKeyDirect, envPath},
		{devconfig.EnvAgentPrivateKey, envPath},
		{devconfig.EnvDID, devPath},
		{devconfig.EnvAgentID, devPath},
		{devconfig.EnvBaseURL, devPath},
		{devconfig.EnvBackendURL, devPath},
	} {
		t.Run(tc.varName, func(t *testing.T) {
			a, _, errb := testApp(map[string]string{tc.varName: "set"})
			a.warnShadowedByEnv(authFields{}, envPath)
			s := errb.String()
			if !strings.Contains(s, tc.varName) {
				t.Errorf("warning should name %s:\n%s", tc.varName, s)
			}
			if !strings.Contains(s, tc.wantFile) {
				t.Errorf("warning for %s should name %s:\n%s", tc.varName, tc.wantFile, s)
			}
		})
	}
}

// Warn, never refuse: exporting these in CI is a documented pattern.
func TestEnvShadowStillWrites(t *testing.T) {
	home := isolateHome(t)
	a, _, _ := testApp(map[string]string{devconfig.EnvAPIKeyDirect: "obx_from_env"})
	if code := a.writeSecrets(filepath.Join(home, ".env"), authFields{apiKey: "obx_written", privateKey: testSeedB64}, nil); code != exitOK {
		t.Fatal("a shadowed field must still be written")
	}
	if readEnvFile(t, home)[devconfig.EnvAPIKeyDirect] != "obx_written" {
		t.Error("the file should hold what auth wrote, regardless of the environment")
	}
}

// --- command wiring ----------------------------------------------------------

func TestAuthIsDispatchedAndInHelp(t *testing.T) {
	a, _, errb := testApp(nil)
	a.usage()
	s := errb.String()
	if !strings.Contains(s, "openbox auth") {
		t.Errorf("usage should list auth:\n%s", s)
	}
	// auth first, init second — the documented order.
	if strings.Index(s, "openbox auth") > strings.Index(s, "openbox init --provider") {
		t.Errorf("auth should be listed before init:\n%s", s)
	}
}

// No flag may accept a secret VALUE (INV-1): flags name sources.
func TestNoAuthFlagTakesASecretValue(t *testing.T) {
	raw, err := os.ReadFile("auth.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	// A StringVar named for a secret would be a value-taking flag.
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
	// The stdin forms must exist, so automation has a path that is not a flag value.
	for _, want := range []string{`"api-key-stdin"`, `"private-key-stdin"`} {
		if !strings.Contains(body, want) {
			t.Errorf("auth.go should offer %s", want)
		}
	}
}

// Piping input with no --*-stdin flag must fail immediately rather than hang: a
// command that blocks on stdin in CI hangs to the job timeout with no output.
func TestNonInteractiveWithoutStdinFlagsFailsFast(t *testing.T) {
	isolateHome(t)
	a, _, errb := testApp(nil)
	a.stdin = strings.NewReader("")
	code := a.run([]string{"auth"})
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(errb.String(), "--api-key-stdin") {
		t.Errorf("error should name the automation flags:\n%s", errb.String())
	}
}

// The stdin automation path: two lines, fixed order, no secret on argv.
func TestStdinAutomationPath(t *testing.T) {
	home := isolateHome(t)
	a, out, errb := testApp(nil)
	a.stdin = strings.NewReader("obx_piped_key\n" + testSeedB64 + "\n")
	code := a.run([]string{
		"auth", "--api-key-stdin", "--private-key-stdin", "--yes",
		"--did", "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301", "--agent-id", "agent-1",
		"--base-url", "https://core.internal", "--backend-url", "https://api.internal",
	})
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", code, exitOK, errb.String(), out.String())
	}
	kv := readEnvFile(t, home)
	if kv[devconfig.EnvAPIKeyDirect] != "obx_piped_key" || kv[devconfig.EnvAgentPrivateKey] != testSeedB64 {
		t.Errorf("piped secrets not written: %v", kv)
	}
	// No secret may reach stdout.
	if strings.Contains(out.String(), "obx_piped_key") || strings.Contains(out.String(), testSeedB64) {
		t.Errorf("a secret reached stdout:\n%s", out.String())
	}
}

// A short read must fail: with both flags set, one supplied line would otherwise
// write an empty signing key over a working one.
func TestStdinShortReadFails(t *testing.T) {
	isolateHome(t)
	a, _, errb := testApp(nil)
	a.stdin = strings.NewReader("obx_only_one_line\n")
	code := a.run([]string{"auth", "--api-key-stdin", "--private-key-stdin", "--yes",
		"--did", "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301"})
	if code != exitError {
		t.Fatalf("exit = %d, want an error for a short read", code)
	}
	if !strings.Contains(errb.String(), "want 2") {
		t.Errorf("error should say how many lines it wanted:\n%s", errb.String())
	}
}

// Validation catches a value in the wrong stdin slot, which is what makes the
// fixed order safe rather than merely documented.
func TestStdinWrongOrderIsCaught(t *testing.T) {
	isolateHome(t)
	a, _, errb := testApp(nil)
	// Signing key first, api key second — swapped.
	a.stdin = strings.NewReader(testSeedB64 + "\nobx_the_api_key\n")
	code := a.run([]string{"auth", "--api-key-stdin", "--private-key-stdin", "--yes",
		"--did", "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301"})
	if code != exitError {
		t.Fatalf("exit = %d, want an error when the values are swapped", code)
	}
	if !strings.Contains(errb.String(), "signing key") {
		t.Errorf("error should point at the signing key:\n%s", errb.String())
	}
}

// Success names the command that actually installs governance: auth alone governs
// nothing, and a user who stops here has telemetry from no session at all.
func TestAuthSuccessNamesInitAsTheNextStep(t *testing.T) {
	isolateHome(t)
	a, out, _ := testApp(nil)
	a.stdin = strings.NewReader("obx_k\n" + testSeedB64 + "\n")
	code := a.run([]string{"auth", "--api-key-stdin", "--private-key-stdin", "--yes",
		"--did", "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301"})
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	s := out.String()
	if !strings.Contains(s, "openbox init") {
		t.Errorf("success output should name `openbox init`:\n%s", s)
	}
	// And it must state the scope default, which is the thing users are most
	// likely to be surprised by.
	if !strings.Contains(s, "THIS DIRECTORY") {
		t.Errorf("success output should state the project-local scope default:\n%s", s)
	}
}

// Registering needs an org credential, and the error must say which kind.
func TestRegisterWithoutAnOrgKeyExplainsWhatIsNeeded(t *testing.T) {
	isolateHome(t)
	a, _, errb := testApp(nil)
	_, _, code := a.registerForAuth(authFields{backendURL: "https://api.internal"}, "", "", "", false)
	if code != exitError {
		t.Fatalf("exit = %d, want an error with no control token", code)
	}
	// The comment above this test promised "the error must say WHICH kind" of
	// credential — and the original only checked the exit code, so replacing the
	// whole four-line message with errors.New("no token") would have passed it.
	// Registering is the one thing that needs an org key, and a user who has an
	// agent id does not need one at all, so both facts have to be in the message.
	s := errb.String()
	if !strings.Contains(s, devconfig.EnvControlToken) {
		t.Errorf("error must name the credential it needs:\n%s", s)
	}
	if !strings.Contains(s, "obx_key_") {
		t.Errorf("error must say which KIND of key (an org key):\n%s", s)
	}
	if !strings.Contains(s, "agent id") {
		t.Errorf("error must offer the alternative — give an existing agent id:\n%s", s)
	}
}

// Registration must reach devinit.Register — NOT devinit.Run — because Run also
// invokes the provider installer and auth must never install hooks.
func TestRegisterWritesCredentialsButInstallsNothing(t *testing.T) {
	home := isolateHome(t)
	a, out, errb := testApp(map[string]string{devconfig.EnvControlToken: "obx_key_" + strings.Repeat("f", 48)})
	a.newRegistrar = func(_, _, _ string) devinit.Registrar {
		return &fakeReg{reg: &backend.Registration{
			AgentID: "srv-agent", DID: "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301",
			APIKey: "obx_minted", PrivateKey: testSeedB64,
		}}
	}
	res, ref, code := a.registerForAuth(authFields{
		backendURL: "https://api.internal", baseURL: "https://core.internal",
	}, "", "desc", "", false)
	if code != exitOK {
		t.Fatalf("exit = %d; stderr=%q", code, errb.String())
	}
	if !res.Registered || ref.AgentID != "srv-agent" {
		t.Fatalf("registration result = %+v ref = %+v", res, ref)
	}
	// It must NOT have applied provider config.
	if res.ConfigApplied {
		t.Error("auth registered AND installed; init owns installation")
	}
	kv := readEnvFile(t, home)
	if kv[devconfig.EnvAPIKeyDirect] != "obx_minted" {
		t.Errorf("minted credentials not written: %v", kv)
	}
	if strings.Contains(out.String(), "obx_minted") || strings.Contains(out.String(), testSeedB64) {
		t.Errorf("a minted secret reached stdout:\n%s", out.String())
	}
}

// The agent-id prompt must NOT pre-fill, even when dev.json holds an id.
//
// The trap: Line() returns its DEFAULT on empty input, so while this prompt
// offered the stored id, pressing Enter at text reading "blank registers a new
// agent" KEPT the old id and fell through to collect mode — demanding an API key
// the user did not have, precisely because they were trying to register one. And
// nothing could express blank: readLine trims only \r\n, so even a space came
// back as a non-empty id. The prompt documented an action it could not accept.
//
// Found by a real first run, not by a test, which is why this one exists.
func TestAgentIDPromptNeverPrefills(t *testing.T) {
	const onFile = "83bb12af-1a76-4ebd-9e9a-989afc40720a"

	t.Run("Enter registers, even with an id on file", func(t *testing.T) {
		p := &prompt.Scripted{Answers: []string{"", "", "", "SHOULD-NOT-BE-READ"}}
		got, err := collectAuthFields(p, authFields{agentID: onFile, did: "did:aip:x"}, false)
		if err != nil {
			t.Fatal(err)
		}
		if !got.register {
			t.Fatal("blank must register — that is what the prompt says it does")
		}
		if got.agentID != "" {
			t.Errorf("agentID = %q, want it cleared for registration", got.agentID)
		}
	})

	// The stored id must not be OFFERED, which is the half a reader cannot see
	// from behaviour alone: a default would appear in the prompt line itself.
	t.Run("the stored id is not shown as a default", func(t *testing.T) {
		p := &prompt.Scripted{Answers: []string{"", "", "", "SHOULD-NOT-BE-READ"}}
		if _, err := collectAuthFields(p, authFields{agentID: onFile}, false); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(p.Out.String(), onFile) {
			t.Errorf("the stored agent id was offered as a prompt default:\n%s", p.Out.String())
		}
	})

	// Reuse is the deliberate act now: type the id.
	t.Run("typing an id reuses that agent", func(t *testing.T) {
		p := &prompt.Scripted{Answers: []string{"", "", onFile, "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301", "obx_k", testSeedB64}}
		got, err := collectAuthFields(p, authFields{}, false)
		if err != nil {
			t.Fatal(err)
		}
		if got.register {
			t.Error("an explicit agent id must not register")
		}
		if got.agentID != onFile {
			t.Errorf("agentID = %q, want %q", got.agentID, onFile)
		}
	})
}
