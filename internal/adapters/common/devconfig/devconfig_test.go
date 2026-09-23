package devconfig

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testDID is a decorative did:aip: value for fixtures unrelated to identity
// resolution (a raw dev.json a bool-flag or posture test writes just to give
// the file a shape); it is never asserted through AttributionDIDFor. Identity
// tests use testAgentID and derive their expected DID from it directly.
const testDID = "did:aip:7f3c9b2e-0000-5000-a000-000000000001"

// testAgentID is the fixed v3 agent id identity tests configure; its
// attribution DID is always computed via AttributionDIDFor rather than
// hardcoded, so a change to the derivation cannot silently desync a test from
// the function it is testing.
const testAgentID = "7f3c9b2e-1111-5000-a000-000000000002"

// testWorkloadKey is a placeholder workload-signing-key value. devconfig never
// parses it (that's workloadauth's job, one layer up), so any non-empty,
// non-secret-shaped string proves the field is wired without needing a real
// RSA key.
const testWorkloadKey = "wk_test_not_a_real_key"

func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv(EnvConfigPath, filepath.Join(t.TempDir(), "none.json"))
	t.Setenv(EnvHome, t.TempDir())
	bindForTest(t, "claude-code")
	for _, name := range append([]string{EnvAPIKeyDirect, EnvWorkloadPrivateKey, EnvAgentID, EnvDID}, legacySeedEnvNames...) {
		t.Setenv(name, "")
	}
	// The legacy seed names are read with LookupEnv, so an empty value still
	// counts as set: unset them, or a developer who still exports one gets a
	// different fixture from one who does not.
	for _, name := range []string{EnvTier2, EnvTier2Timeout, EnvRequireVerified} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

// bindForTest binds for the length of one case. Every production caller of
// ResolveCredentials runs under a command that has already said which tool it
// is acting for; a fixture that left this unbound would exercise a path no
// command takes, and after the unbound gate it does not resolve at all.
func bindForTest(t *testing.T, tool string) {
	t.Helper()
	release, err := BindProvider(tool)
	if err != nil {
		t.Fatalf("bind %s: %v", tool, err)
	}
	t.Cleanup(release)
}

func writeEnvFileForTest(t *testing.T, kv map[string]string) {
	t.Helper()
	path, err := EnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteEnvFile(path, kv); err != nil {
		t.Fatal(err)
	}
}

// wantDID is AttributionDIDFor(agentID), fatal on error. Every identity test
// derives its expectation this way rather than hardcoding a did:aip: literal,
// so a change to the derivation cannot silently desync a test from the
// function it exercises.
func wantDID(t *testing.T, agentID string) string {
	t.Helper()
	did, err := AttributionDIDFor(agentID)
	if err != nil {
		t.Fatalf("AttributionDIDFor(%q): %v", agentID, err)
	}
	return did
}

// TestResolveDIDDerivesFromAgentID pins the flip (phase 04 D1): the DID is no
// longer read from a store, it is derived from the agent id -- env first,
// dev.json's agent_id as the fallback.
func TestResolveDIDDerivesFromAgentID(t *testing.T) {
	t.Run("env", func(t *testing.T) {
		isolateConfig(t)
		t.Setenv(EnvAgentID, testAgentID)
		did, err := ResolveDID()
		if err != nil {
			t.Fatalf("resolve DID: %v", err)
		}
		if want := wantDID(t, testAgentID); did != want {
			t.Errorf("DID = %q, want %q", did, want)
		}
	})

	t.Run("config file fallback", func(t *testing.T) {
		cfgPath := filepath.Join(t.TempDir(), "dev.json")
		if err := os.WriteFile(cfgPath, []byte(`{"agent_id":"`+testAgentID+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(EnvConfigPath, cfgPath)
		t.Setenv(EnvAgentID, "") // no env override → must come from the file
		did, err := ResolveDID()
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if want := wantDID(t, testAgentID); did != want {
			t.Errorf("DID from config = %q, want %q", did, want)
		}
	})

	t.Run("neither configures one", func(t *testing.T) {
		isolateConfig(t)
		if _, err := ResolveDID(); err == nil {
			t.Error("expected an error when no agent id is configured")
		} else if !errors.Is(err, ErrMissingAgentID) {
			t.Errorf("error = %v, want ErrMissingAgentID", err)
		}
	})
}

// TestResolveDIDRefusesLegacyStore a stored developer_did means the store
// predates v3 entirely: ResolveDID must refuse with ErrLegacyStore rather than
// derive an answer that would silently disagree with what's on disk.
func TestResolveDIDRefusesLegacyStore(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	if err := os.WriteFile(cfgPath, []byte(`{"developer_did":"`+testDID+`","agent_id":"`+testAgentID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvConfigPath, cfgPath)
	if _, err := ResolveDID(); !errors.Is(err, ErrLegacyStore) {
		t.Errorf("err = %v, want ErrLegacyStore", err)
	}
	if got := ResolveDIDOrEmpty(); got != "" {
		t.Errorf("ResolveDIDOrEmpty() = %q on a legacy store, want \"\"", got)
	}
	if baseURL, did := ResolveCoordinates(); did != "" {
		t.Errorf("ResolveCoordinates() did = %q on a legacy store, want \"\" (base url %q)", did, baseURL)
	}
}

// TestAgentDIDEnvIsIgnored OPENBOX_AGENT_DID is retired: it named a v1 DID
// that could disagree with the store, which is exactly the two-store bug the
// derivation exists to make impossible.
func TestAgentDIDEnvIsIgnored(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvAgentID, testAgentID)
	t.Setenv(EnvDID, "did:aip:some-other-value-the-resolver-must-never-return")

	did, err := ResolveDID()
	if err != nil {
		t.Fatalf("resolve DID: %v", err)
	}
	if want := wantDID(t, testAgentID); did != want {
		t.Errorf("DID = %q, want the derived value %q; OPENBOX_AGENT_DID must be ignored", did, want)
	}
}

// TestResolveCredentials_RealEnvBeatsFile the real environment variable beats
// the credential file; that is what lets CI supply credentials without writing
// anything to disk.
func TestResolveCredentials_RealEnvBeatsFile(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvAgentID, testAgentID)
	writeEnvFileForTest(t, map[string]string{
		EnvAPIKeyDirect:       "obx_from_file",
		EnvWorkloadPrivateKey: "wk_from_file",
	})
	t.Setenv(EnvAPIKeyDirect, "obx_from_env")
	t.Setenv(EnvWorkloadPrivateKey, "wk_from_env")
	t.Setenv(EnvBaseURL, "https://core.example.ai")
	t.Setenv(EnvContentCapture, "true")

	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.APIKey != "obx_from_env" || c.WorkloadPrivateKey != "wk_from_env" {
		t.Errorf("creds = %+v, want the environment to win over the file", c)
	}
	if c.BaseURL != "https://core.example.ai" {
		t.Errorf("base url = %q", c.BaseURL)
	}
	if !c.ContentCaptureEnabled {
		t.Error("content capture should be enabled")
	}
	if want := wantDID(t, testAgentID); c.DID != want {
		t.Errorf("DID = %q, want %q", c.DID, want)
	}
}

// TestResolveCredentials_FromCredentialFile with no environment override, both
// secrets come from ~/.openbox/.env; the position the deleted OS secret store
// used to hold. The agent id comes from dev.json, since it is a coordinate,
// never the secrets file.
func TestResolveCredentials_FromCredentialFile(t *testing.T) {
	isolateConfig(t)
	if err := WriteConfig(mustDevConfigPath(t), Update{AgentID: testAgentID}); err != nil {
		t.Fatal(err)
	}
	writeEnvFileForTest(t, map[string]string{
		EnvAPIKeyDirect:       "obx_from_file",
		EnvWorkloadPrivateKey: "wk_from_file",
	})

	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.APIKey != "obx_from_file" || c.WorkloadPrivateKey != "wk_from_file" {
		t.Errorf("creds from file = %+v", c)
	}
	if c.BaseURL != DefaultBaseURL {
		t.Errorf("base url default = %q, want %q", c.BaseURL, DefaultBaseURL)
	}
	if want := wantDID(t, testAgentID); c.DID != want {
		t.Errorf("DID = %q, want %q", c.DID, want)
	}
}

// mustDevConfigPath is DefaultConfigPath(), fatal on nothing to resolve
// (isolateConfig always binds, so this always has an answer in tests).
func mustDevConfigPath(t *testing.T) string {
	t.Helper()
	return DefaultConfigPath()
}

// TestEnvFileIsNotACoordinateSource the tripwire for the second-store bug
// (that decision, D2), rewritten for the v3 shape: the credential file
// carries the two secrets and, deliberately, a coordinate-shaped value too;
// resolution must reach the coordinate gate on the agent id from env/dev.json
// alone, never from anything sitting in .env.
//
// (a) proves ResolveCredentials fails on the MISSING AGENT ID, not a missing
// secret, when the file's coordinate is the only thing that could have
// supplied one.
// (b) proves the real environment variable, not the file's value, is what
// answers once it is set, and that the file's OPENBOX_BASE_URL is likewise
// never read as a coordinate.
// (c) is the guard: with the two secrets removed, the same fixture must fail
// for a MISSING SECRET instead, proving (a) actually reached the coordinate
// gate rather than failing earlier for an unrelated reason.
func TestEnvFileIsNotACoordinateSource(t *testing.T) {
	uuidA := "aaaaaaaa-0000-5000-a000-00000000000a"
	uuidB := "bbbbbbbb-0000-5000-a000-00000000000b"

	t.Run("a: fails for the missing agent id, not a missing secret", func(t *testing.T) {
		isolateConfig(t)
		writeEnvFileForTest(t, map[string]string{
			EnvAPIKeyDirect:       "obx_k",
			EnvWorkloadPrivateKey: "wk_k",
			EnvAgentID:            uuidA,
			EnvBaseURL:            "https://wrong.example",
		})
		_, err := ResolveCredentials()
		if err == nil {
			t.Fatal("an agent id in .env was treated as a coordinate source; that is the two-store bug that decision removed")
		}
		if !errors.Is(err, ErrMissingAgentID) {
			t.Errorf("err = %v, want ErrMissingAgentID (the resolver must have reached the coordinate gate, not a secret gate)", err)
		}
	})

	t.Run("b: the real environment variable answers, never the file's", func(t *testing.T) {
		isolateConfig(t)
		writeEnvFileForTest(t, map[string]string{
			EnvAPIKeyDirect:       "obx_k",
			EnvWorkloadPrivateKey: "wk_k",
			EnvAgentID:            uuidA,
			EnvBaseURL:            "https://wrong.example",
		})
		t.Setenv(EnvAgentID, uuidB)

		c, err := ResolveCredentials()
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		want := wantDID(t, uuidB)
		if c.DID != want {
			t.Errorf("DID = %q, want the environment agent id's %q, never uuid A's %q", c.DID, want, wantDID(t, uuidA))
		}
		if c.BaseURL != DefaultBaseURL {
			t.Errorf("base URL = %q, want the built-in default; .env must not supply a coordinate", c.BaseURL)
		}
	})

	t.Run("c: guard -- without the file's secrets, the same fixture fails for a secret instead", func(t *testing.T) {
		isolateConfig(t)
		writeEnvFileForTest(t, map[string]string{
			EnvAgentID: uuidA,
			EnvBaseURL: "https://wrong.example",
		})
		_, err := ResolveCredentials()
		if err == nil {
			t.Fatal("expected an error")
		}
		if errors.Is(err, ErrMissingAgentID) {
			t.Errorf("err = %v, want a missing-secret error, not ErrMissingAgentID: this proves (a) reached the coordinate gate rather than short-circuiting earlier", err)
		}
	})
}

// TestCredentialFileAloneIsNotSufficientForAgentID a .env alone is
// deliberately not enough to run: an agent id is a coordinate, and the file
// never supplies one, so the failure names the missing agent id, not
// something obscure later.
func TestCredentialFileAloneIsNotSufficientForAgentID(t *testing.T) {
	isolateConfig(t)
	writeEnvFileForTest(t, map[string]string{
		EnvAPIKeyDirect:       "obx_k",
		EnvWorkloadPrivateKey: "wk_k",
	})
	_, err := ResolveCredentials()
	if err == nil {
		t.Fatal("expected the missing-agent-id error")
	}
	if !errors.Is(err, ErrMissingAgentID) {
		t.Errorf("err = %v, want ErrMissingAgentID", err)
	}
}

// TestResolveCredentials_CRLFStrippedFromCredentialFile a crlf-authored .env
// must not leave \r on the workload signing key: it would fail assertion
// signing with an error naming neither the file nor the character.
func TestResolveCredentials_CRLFStrippedFromCredentialFile(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvAgentID, testAgentID)
	path, err := EnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := EnvAPIKeyDirect + "=obx_k\r\n" + EnvWorkloadPrivateKey + "=YmFzZTY0\r\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.WorkloadPrivateKey != "YmFzZTY0" {
		t.Errorf("workload key = %q, want no trailing carriage return", c.WorkloadPrivateKey)
	}
}

// TestSeedAliasesNoLongerSatisfyIdentity an Ed25519 seed, under the documented
// name or a deprecated alias, never satisfies a v3 identity: it either marks
// the store legacy (found in the credential file, LegacyStoreFor's own
// signal) or, exported only as an environment variable with no matching file
// marker, simply is not a workload key, so resolution fails on the missing
// one exactly as if nothing were exported at all.
func TestSeedAliasesNoLongerSatisfyIdentity(t *testing.T) {
	for _, alias := range legacySeedEnvNames {
		t.Run(alias+"/file is legacy", func(t *testing.T) {
			isolateConfig(t)
			t.Setenv(EnvAgentID, testAgentID)
			writeEnvFileForTest(t, map[string]string{EnvAPIKeyDirect: "obx_k", alias: "c2VlZA=="})
			_, err := ResolveCredentials()
			if !errors.Is(err, ErrLegacyStore) {
				t.Errorf("err = %v, want ErrLegacyStore (a seed in the credential file marks the store legacy)", err)
			}
		})
		t.Run(alias+"/env alone is not a workload key", func(t *testing.T) {
			isolateConfig(t)
			t.Setenv(EnvAgentID, testAgentID)
			t.Setenv(EnvAPIKeyDirect, "obx_k")
			t.Setenv(alias, "c2VlZA==")
			_, err := ResolveCredentials()
			if err == nil {
				t.Fatal("a seed alias satisfied a v3 identity with no workload key present")
			}
			if errors.Is(err, ErrLegacyStore) {
				t.Errorf("err = %v, want a missing-workload-key error: an exported alias with no file marker is not a legacy STORE", err)
			}
		})
	}
}

// TestCredentialsRequireWorkloadKey an API key alone, with an agent id and no
// workload key anywhere, is not a complete v3 identity.
func TestCredentialsRequireWorkloadKey(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvAgentID, testAgentID)
	t.Setenv(EnvAPIKeyDirect, "obx_k")
	_, err := ResolveCredentials()
	if err == nil {
		t.Fatal("expected an error when no workload key is configured")
	}
	if !strings.Contains(err.Error(), EnvWorkloadPrivateKey) {
		t.Errorf("error = %q, want it to name %s", err, EnvWorkloadPrivateKey)
	}
}

func TestResolveCredentials_MissingSecret(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvAgentID, testAgentID)
	_, err := ResolveCredentials()
	if err == nil {
		t.Fatal("expected an error when no api key source is configured")
	}
	// The remedy moved with the credential: `auth` connects the organization and
	// writes no agent key, so pointing a user there would send them somewhere
	// that cannot fix this.
	if !strings.Contains(err.Error(), "openbox init --provider") {
		t.Errorf("error = %q, want it to name `openbox init --provider`", err)
	}
	if strings.Contains(err.Error(), "openbox auth") {
		t.Errorf("error still names `openbox auth`, which no longer writes an agent credential: %q", err)
	}
}

// TestTheMissingCredentialRemedyNamesTheBoundTool a message that says
// "<tool>" when it could say "codex" makes the reader do the substitution, and
// this one is read exactly when the machine is already not working.
func TestTheMissingCredentialRemedyNamesTheBoundTool(t *testing.T) {
	isolateConfig(t)
	release, err := BindProvider("codex")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	_, err = ResolveCredentials()
	if err == nil {
		t.Fatal("expected an error when no api key source is configured")
	}
	if !strings.Contains(err.Error(), "openbox init --provider codex") {
		t.Errorf("error = %q, want it to name the bound tool", err)
	}
}

// TestResolveCredentials_UnparseableFileIsAnError an unparseable credential
// file must not read as "no credentials": that would send a user hunting for a
// registration problem they do not have.
func TestResolveCredentials_UnparseableFileIsAnError(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvAgentID, testAgentID)
	path, err := EnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	dup := EnvAPIKeyDirect + "=obx_one\n" + EnvAPIKeyDirect + "=obx_two\n"
	if err := os.WriteFile(path, []byte(dup), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveCredentials(); err == nil {
		t.Fatal("a duplicate-key credential file resolved as if it were empty")
	}
}

// TestEnvIdentityPresentMeansWorkloadKey EnvIdentityPresent now asks the v3
// question outright: an exported seed (current or deprecated alias), with no
// workload key, must not read as a present identity.
func TestEnvIdentityPresentMeansWorkloadKey(t *testing.T) {
	isolateConfig(t)
	if EnvIdentityPresent() {
		t.Error("nothing exported; must be false")
	}
	t.Setenv(EnvAPIKeyDirect, "obx_k")
	if EnvIdentityPresent() {
		t.Error("API key alone must not be enough")
	}
	t.Setenv(EnvAgentPrivateKey, "c2VlZA==")
	if EnvIdentityPresent() {
		t.Error("a seed under the documented legacy name must not satisfy a v3 identity")
	}
	t.Setenv(EnvAgentPrivateKey, "")
	t.Setenv(EnvWorkloadPrivateKey, "wk_k")
	if !EnvIdentityPresent() {
		t.Error("an API key + a workload key must be a present identity")
	}
}

func TestResolveContentCapture_DefaultOn(t *testing.T) {
	isolateConfig(t)
	if !ResolveContentCapture() {
		t.Error("ResolveContentCapture default must be ON (absent config)")
	}
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	_ = os.WriteFile(cfgPath, []byte(`{"developer_did":"`+testDID+`","content_capture":false}`), 0o600)
	t.Setenv(EnvConfigPath, cfgPath)
	if ResolveContentCapture() {
		t.Error("explicit content_capture:false must opt out")
	}
	t.Setenv(EnvContentCapture, "0")
	if ResolveContentCapture() {
		t.Error("OPENBOX_CONTENT_CAPTURE=0 must force OFF")
	}
}

// TestResolveFinops_DefaultOn pins that decision posture flip, and it pins the
// absent-field case specifically; which is the case the old implementation
// could not express.
func TestResolveFinops_DefaultOn(t *testing.T) {
	isolateConfig(t)
	if !ResolveFinops() {
		t.Error("ResolveFinops default must be ON (absent config field)")
	}

	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	_ = os.WriteFile(cfgPath, []byte(`{"developer_did":"`+testDID+`"}`), 0o600)
	t.Setenv(EnvConfigPath, cfgPath)
	if !ResolveFinops() {
		t.Error("a config file with no finops key must resolve to the default (ON), not to false")
	}

	_ = os.WriteFile(cfgPath, []byte(`{"developer_did":"`+testDID+`","finops":false}`), 0o600)
	if ResolveFinops() {
		t.Error("explicit finops:false must opt out")
	}

	t.Setenv(EnvFinops, "1")
	if !ResolveFinops() {
		t.Error("OPENBOX_FINOPS=1 must override config false")
	}
	_ = os.WriteFile(cfgPath, []byte(`{"developer_did":"`+testDID+`","finops":true}`), 0o600)
	t.Setenv(EnvFinops, "0")
	if ResolveFinops() {
		t.Error("OPENBOX_FINOPS=0 must force OFF")
	}
}

// TestPostureReportsEffectiveFinopsState the posture record must report the
// same state the resolver does, or the evidence an auditor reads contradicts
// what the session actually did; which is worse than having no evidence,
// because it is trusted.
func TestPostureReportsEffectiveFinopsState(t *testing.T) {
	isolateConfig(t)
	if got := EffectivePosture().Finops; got != ResolveFinops() {
		t.Errorf("posture finops = %t but ResolveFinops() = %t (absent config)", got, ResolveFinops())
	}
	if !EffectivePosture().Finops {
		t.Error("posture must record finops ON at the default posture")
	}

	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	_ = os.WriteFile(cfgPath, []byte(`{"developer_did":"`+testDID+`","finops":false}`), 0o600)
	t.Setenv(EnvConfigPath, cfgPath)
	p := EffectivePosture()
	if p.Finops {
		t.Error("posture must record finops OFF when the config opts out")
	}
	if p.Finops != ResolveFinops() {
		t.Errorf("posture finops = %t but ResolveFinops() = %t (config opt-out)", p.Finops, ResolveFinops())
	}
	if got, ok := p.Flags()["finops"]; !ok || got {
		t.Errorf("Flags()[finops] = (%t, present=%t), want (false, true)", got, ok)
	}

	t.Setenv(EnvFinops, "1")
	if !EffectivePosture().Finops {
		t.Error("posture must follow the env override")
	}
}

func TestResolveRealtime_DefaultOn(t *testing.T) {
	isolateConfig(t)
	if !ResolveRealtime() {
		t.Error("ResolveRealtime default must be ON (absent config)")
	}
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	_ = os.WriteFile(cfgPath, []byte(`{"developer_did":"`+testDID+`","realtime_flush":false}`), 0o600)
	t.Setenv(EnvConfigPath, cfgPath)
	if ResolveRealtime() {
		t.Error("explicit realtime_flush:false must opt out")
	}
	t.Setenv(EnvRealtime, "1")
	if !ResolveRealtime() {
		t.Error("OPENBOX_REALTIME=1 must override config false")
	}
	_ = os.WriteFile(cfgPath, []byte(`{"developer_did":"`+testDID+`","realtime_flush":true}`), 0o600)
	t.Setenv(EnvRealtime, "0")
	if ResolveRealtime() {
		t.Error("OPENBOX_REALTIME=0 must force OFF")
	}
}

func TestBoolFlagPrecedence(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	write := func(json string) { _ = os.WriteFile(cfgPath, []byte(json), 0o600) }
	t.Setenv(EnvConfigPath, cfgPath)
	os.Unsetenv(EnvEnforce)

	// Safe because enforcement is inert without an org policy and fail_closed
	// stays off, so an outage never blocks a tool call.
	write(`{"developer_did":"` + testDID + `"}`)
	if !ResolveEnforce() {
		t.Error("an absent enforce field must resolve to ON ")
	}
	write(`{"developer_did":"` + testDID + `","enforce":false}`)
	if ResolveEnforce() {
		t.Error("enforce:false in config must opt out")
	}
	write(`{"developer_did":"` + testDID + `","enforce":true}`)
	if !ResolveEnforce() {
		t.Error("enforce:true in config should enable")
	}
	t.Setenv(EnvEnforce, "false")
	if ResolveEnforce() {
		t.Error("env false must override config true")
	}
	write(`{"developer_did":"` + testDID + `"}`)
	t.Setenv(EnvEnforce, "1")
	if !ResolveEnforce() {
		t.Error("env 1 must override config absent/false")
	}
}

func TestResolveTimeoutMS(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	t.Setenv(EnvConfigPath, cfgPath)
	os.Unsetenv(EnvTier2Timeout)
	field := func(c DevConfig) int { return c.Tier2TimeoutMS }

	_ = os.WriteFile(cfgPath, []byte(`{}`), 0o600)
	if ms := ResolveTimeoutMS(field, EnvTier2Timeout); ms != 0 {
		t.Errorf("unset = %d, want 0", ms)
	}
	_ = os.WriteFile(cfgPath, []byte(`{"tier2_timeout_ms":250}`), 0o600)
	if ms := ResolveTimeoutMS(field, EnvTier2Timeout); ms != 250 {
		t.Errorf("config = %d, want 250", ms)
	}
	t.Setenv(EnvTier2Timeout, "500")
	if ms := ResolveTimeoutMS(field, EnvTier2Timeout); ms != 500 {
		t.Errorf("env = %d, want 500", ms)
	}
	t.Setenv(EnvTier2Timeout, "not-a-number")
	if ms := ResolveTimeoutMS(field, EnvTier2Timeout); ms != 250 {
		t.Errorf("garbage env should fall back to config 250, got %d", ms)
	}
}

func TestSpoolDir(t *testing.T) {
	t.Setenv(EnvSpoolDir, "/pinned/spool")
	if d := SpoolDir("codex-spool"); d != "/pinned/spool" {
		t.Errorf("env override = %q", d)
	}
	os.Unsetenv(EnvSpoolDir)
	t.Setenv(EnvSpoolDir, "")
	d := SpoolDir("codex-spool")
	if filepath.Base(d) != "codex-spool" || filepath.Base(filepath.Dir(d)) != "openbox" {
		t.Errorf("default spool dir = %q, want …/openbox/codex-spool", d)
	}
}

// TestTelemetryOptOutSurvivesARoundTrip is why Telemetry is a *bool.
// `omitempty` drops a plain `false`, so an org's deliberate `telemetry:false`
// would vanish from the file the next time anything rewrote it; and since the
// default is ON, the lane would silently switch itself back on.
func TestTelemetryOptOutSurvivesARoundTrip(t *testing.T) {
	off := false
	raw, err := json.Marshal(DevConfig{Telemetry: &off})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"telemetry":false`) {
		t.Fatalf("an explicit opt-out did not survive the write: %s", raw)
	}

	var back DevConfig
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Telemetry == nil {
		t.Fatal("the opt-out read back as ABSENT, which resolves to the ON default")
	}
	if *back.Telemetry {
		t.Fatal("the opt-out read back as true")
	}

	bare, err := json.Marshal(DevConfig{})
	if err != nil {
		t.Fatalf("marshal bare: %v", err)
	}
	if strings.Contains(string(bare), "telemetry") {
		t.Errorf("an unset field was written anyway: %s", bare)
	}
}

// TestResolveTelemetryDefaultsOnAndEnvWins pins the posture resolution.
func TestResolveTelemetryDefaultsOnAndEnvWins(t *testing.T) {
	isolateConfig(t)

	if !ResolveTelemetry() {
		t.Error("default is OFF; installing the lane is the opt-in, so it must default ON")
	}
	t.Setenv(EnvTelemetry, "0")
	if ResolveTelemetry() {
		t.Errorf("%s=0 did not switch the lane off", EnvTelemetry)
	}
	t.Setenv(EnvTelemetry, "1")
	if !ResolveTelemetry() {
		t.Errorf("%s=1 did not switch the lane on", EnvTelemetry)
	}
}

// TestControlTokenComesFromTheOrgFileWhenTheEnvIsEmpty the org control token
// is persisted now (owner ruling O1): `auth` writes it once and `init` reads
// it later, in a different process, with no exported variable between them.
// The environment still outranks the file so an operator can override a run
// without editing anything.
func TestControlTokenComesFromTheOrgFileWhenTheEnvIsEmpty(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvControlToken, "")

	org, err := OrgEnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteEnvFile(org, map[string]string{EnvControlToken: "obx_key_from_the_file"}); err != nil {
		t.Fatal(err)
	}

	if got := ResolveControlToken(); got != "obx_key_from_the_file" {
		t.Fatalf("ResolveControlToken() = %q, want the value in the org .env", got)
	}

	// Inverted with the contract: the org file now outranks the environment for
	// this one credential, because `auth` writes it and `init` reads it in a
	// separate process, and a stale export silently broke that handoff. See
	// TestTheOrgFileOutranksAStaleExportForTheControlToken for why, and for the
	// fallback that keeps the CI and keep-it-off-disk routes working.
	t.Setenv(EnvControlToken, "obx_key_from_the_env")
	if got := ResolveControlToken(); got != "obx_key_from_the_file" {
		t.Fatalf("ResolveControlToken() = %q, want the file to outrank the environment", got)
	}
}

// TestControlTokenStaysOrgLevelWhileBound the token authorizes agent creation
// across the whole organization; a per-tool store must never be able to supply
// one, or a compromised tool store becomes a fleet compromise.
func TestControlTokenStaysOrgLevelWhileBound(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvControlToken, "")

	org, err := OrgEnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteEnvFile(org, map[string]string{EnvControlToken: "obx_key_org"}); err != nil {
		t.Fatal(err)
	}
	perTool, err := EnvFilePathFor("codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteEnvFile(perTool, map[string]string{EnvControlToken: "obx_key_planted_in_the_tool_store"}); err != nil {
		t.Fatal(err)
	}

	release, err := BindProvider("codex")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if got := ResolveControlToken(); got != "obx_key_org" {
		t.Fatalf("ResolveControlToken() = %q while bound to codex, want the org value; a tool store must not supply a fleet credential", got)
	}
}

// TestCredentialResolutionReportsAnUnresolvablePath loadSecretFile used to
// swallow the path error and return an empty map, which reads downstream as
// "no credentials": a machine that cannot resolve its home would send its
// owner hunting for a registration problem it does not have. The comment on
// that function always said this; the code did not.
func TestCredentialResolutionReportsAnUnresolvablePath(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvDID, testDID)
	t.Setenv(EnvHome, "relative/openbox")

	_, err := ResolveCredentials()
	if err == nil {
		t.Fatal("ResolveCredentials() succeeded with an unresolvable OPENBOX_HOME")
	}
	if !strings.Contains(err.Error(), EnvHome) {
		t.Fatalf("ResolveCredentials() error = %v, want it to name %s rather than report a missing credential", err, EnvHome)
	}
}

// TestTheTokenSourceIsNameable the environment outranks the org file, which is
// correct and is how a CI job overrides one run -- but it also means a stale
// `export OPENBOX_CONTROL_TOKEN=…` left in a long-lived shell silently beats
// every `openbox auth` and every hand edit of the file. Measured on a real
// machine: a developer re-ran auth twice and edited the file in vim, and each
// attempt was refused by the backend, because a token exported twelve days
// earlier was the one being sent. Nothing printed said where the token came
// from, so the file was the only thing they could think to change.
func TestTheTokenSourceIsNameable(t *testing.T) {
	isolateConfig(t)
	org, err := OrgEnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteEnvFile(org, map[string]string{EnvControlToken: "obx_key_from_the_file"}); err != nil {
		t.Fatal(err)
	}

	t.Setenv(EnvControlToken, "")
	tok, src := ResolveControlTokenWithSource()
	if tok != "obx_key_from_the_file" {
		t.Fatalf("token = %q, want the file's", tok)
	}
	if src != org {
		t.Errorf("source = %q, want the org file path %q", src, org)
	}

	// The environment is the fallback now, so it is named only when the file has
	// nothing to offer.
	if err := os.Remove(org); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvControlToken, "obx_key_from_the_env")
	tok, src = ResolveControlTokenWithSource()
	if tok != "obx_key_from_the_env" {
		t.Fatalf("token = %q, want the environment when no file has one", tok)
	}
	if !strings.Contains(src, EnvControlToken) || !strings.Contains(src, "environment") {
		t.Errorf("source = %q, want it to name the environment variable", src)
	}

	// And with nothing anywhere, there is no source to name.
	t.Setenv(EnvControlToken, "")
	if tok, src = ResolveControlTokenWithSource(); tok != "" || src != "" {
		t.Errorf("with no token configured, got %q from %q", tok, src)
	}

	// The one-value form stays the same question, so the two cannot drift.
	t.Setenv(EnvControlToken, "obx_key_again")
	if got := ResolveControlToken(); got != "obx_key_again" {
		t.Errorf("ResolveControlToken() = %q, want it to agree with the sourced form", got)
	}
}

// TestTheOrgFileOutranksAStaleExportForTheControlToken the control token is
// handed between two commands: `auth` writes it, `init` reads it, in separate
// processes. While the environment outranked the file, an export could silently
// break that handoff -- and the export that does it is rarely a deliberate one.
// Measured on a real machine: a GUI editor launched four weeks earlier held one
// in its environment, so every integrated terminal it spawned inherited a stale
// token, `auth` wrote the correct one, `init` sent the stale one, and no amount
// of re-running `auth` or editing the file could change what was sent.
//
// So the file wins when it has a token. The environment is not ignored: it is
// the fallback, which keeps the two routes that depend on it working -- a CI
// image that never runs `auth` has no file, and a developer who declines the
// token prompt to keep it off disk leaves none in the file either.
func TestTheOrgFileOutranksAStaleExportForTheControlToken(t *testing.T) {
	isolateConfig(t)
	org, err := OrgEnvFilePath()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("the file wins when it has one", func(t *testing.T) {
		if err := WriteEnvFile(org, map[string]string{EnvControlToken: "obx_key_from_the_file"}); err != nil {
			t.Fatal(err)
		}
		t.Setenv(EnvControlToken, "obx_key_stale_from_a_shell")
		tok, src := ResolveControlTokenWithSource()
		if tok != "obx_key_from_the_file" {
			t.Errorf("token = %q, want the file's; a stale export must not beat what `auth` just wrote", tok)
		}
		if src != org {
			t.Errorf("source = %q, want %q", src, org)
		}
	})

	t.Run("the environment is the fallback, not ignored", func(t *testing.T) {
		if err := os.Remove(org); err != nil {
			t.Fatal(err)
		}
		t.Setenv(EnvControlToken, "obx_key_from_the_env")
		tok, src := ResolveControlTokenWithSource()
		if tok != "obx_key_from_the_env" {
			t.Errorf("token = %q, want the environment when no file has one", tok)
		}
		if !strings.Contains(src, EnvControlToken) {
			t.Errorf("source = %q, want it to name the variable", src)
		}
	})

	t.Run("a file with no token falls through to the environment", func(t *testing.T) {
		// What `auth` leaves when the token prompt is answered blank: the file
		// exists and holds no token, which must not shadow an export.
		if err := WriteEnvFile(org, map[string]string{"OPENBOX_SOMETHING_ELSE": "x"}); err != nil {
			t.Fatal(err)
		}
		t.Setenv(EnvControlToken, "obx_key_from_the_env")
		if tok, _ := ResolveControlTokenWithSource(); tok != "obx_key_from_the_env" {
			t.Errorf("token = %q, want the environment; an empty file must not shadow it", tok)
		}
	})
}
