package devconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testDID = "did:aip:7f3c9b2e-0000-5000-a000-000000000001"

// isolateConfig points OPENBOX_CONFIG at a nonexistent temp path so tests never
// read the developer machine's real ~/.config/openbox/dev.json.
// isolateConfig points both the dev config and the credential file at empty temp
// locations, so no test reads (or writes) the developer's real ~/.openbox.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv(EnvConfigPath, filepath.Join(t.TempDir(), "none.json"))
	t.Setenv(EnvHome, t.TempDir())
	for _, name := range append([]string{EnvAPIKeyDirect, EnvAgentPrivateKey}, deprecatedPrivateKeyEnvNames...) {
		t.Setenv(name, "")
	}
}

// writeEnvFileForTest writes a credential file under the isolated OPENBOX_HOME.
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

func TestResolveDID(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvDID, testDID)
	did, err := ResolveDID()
	if err != nil {
		t.Fatalf("resolve DID: %v", err)
	}
	if did != testDID {
		t.Errorf("DID = %q, want %q", did, testDID)
	}

	t.Setenv(EnvDID, "")
	if _, err := ResolveDID(); err == nil {
		t.Error("expected error when no DID configured")
	}
}

func TestResolveDIDFromConfigFile(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	if err := os.WriteFile(cfgPath, []byte(`{"developer_did":"`+testDID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvConfigPath, cfgPath)
	t.Setenv(EnvDID, "") // no env override → must come from the file
	did, err := ResolveDID()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if did != testDID {
		t.Errorf("DID from config = %q, want %q", did, testDID)
	}
}

// The real environment variable beats the credential file — that is what lets CI
// supply credentials without writing anything to disk.
func TestResolveCredentials_RealEnvBeatsFile(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvDID, testDID)
	writeEnvFileForTest(t, map[string]string{
		EnvAPIKeyDirect:    "obx_from_file",
		EnvAgentPrivateKey: "ZmlsZQ==",
	})
	t.Setenv(EnvAPIKeyDirect, "obx_from_env")
	t.Setenv(EnvAgentPrivateKey, "ZW52")
	t.Setenv(EnvBaseURL, "https://core.example.ai")
	t.Setenv(EnvContentCapture, "true")

	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.APIKey != "obx_from_env" || c.PrivateKeyB64 != "ZW52" {
		t.Errorf("creds = %+v, want the environment to win over the file", c)
	}
	if c.BaseURL != "https://core.example.ai" {
		t.Errorf("base url = %q", c.BaseURL)
	}
	if !c.ContentCaptureEnabled {
		t.Error("content capture should be enabled")
	}
}

// With no environment override, both secrets come from ~/.openbox/.env — the
// position the deleted OS secret store used to hold.
func TestResolveCredentials_FromCredentialFile(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvDID, testDID)
	writeEnvFileForTest(t, map[string]string{
		EnvAPIKeyDirect:    "obx_from_file",
		EnvAgentPrivateKey: "ZmlsZQ==",
	})

	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.APIKey != "obx_from_file" || c.PrivateKeyB64 != "ZmlsZQ==" {
		t.Errorf("creds from file = %+v", c)
	}
	if c.BaseURL != DefaultBaseURL {
		t.Errorf("base url default = %q, want %q", c.BaseURL, DefaultBaseURL)
	}
}

// THE TRIPWIRE for the second-store bug (ADR-0015, D2). The credential file is
// for secrets only: a coordinate written into it must be ignored, while the same
// coordinate as a real environment variable is honoured. Before ADR-0015 the DID
// lived in two stores and a stale copy silently reverted a corrected one.
//
// If this test is ever deleted or relaxed, that bug class is back. Adding
// coordinates to .env is a decision to reopen, not a commit to make.
func TestEnvFileIsNotACoordinateSource(t *testing.T) {
	isolateConfig(t)
	writeEnvFileForTest(t, map[string]string{
		EnvAPIKeyDirect:    "obx_k",
		EnvAgentPrivateKey: "ZmlsZQ==",
		EnvDID:             "did:aip:from-the-env-file",
		EnvBaseURL:         "https://wrong.example",
	})

	// No DID anywhere else ⇒ the one in .env must NOT rescue it.
	t.Setenv(EnvDID, "")
	if _, err := ResolveCredentials(); err == nil {
		t.Fatal("a DID in .env was treated as a coordinate source; that is the two-store bug ADR-0015 removed")
	}

	// The same DID as a real environment variable IS honoured.
	t.Setenv(EnvDID, testDID)
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.DID != testDID {
		t.Errorf("DID = %q, want the environment value %q", c.DID, testDID)
	}
	if c.BaseURL != DefaultBaseURL {
		t.Errorf("base URL = %q, want the built-in default — .env must not supply a coordinate", c.BaseURL)
	}
}

// A .env alone is deliberately NOT enough to run: it carries no DID, so the
// failure is the clear no-DID error rather than something obscure later.
func TestCredentialFileAloneIsNotSufficient(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvDID, "")
	writeEnvFileForTest(t, map[string]string{
		EnvAPIKeyDirect:    "obx_k",
		EnvAgentPrivateKey: "ZmlsZQ==",
	})
	_, err := ResolveCredentials()
	if err == nil {
		t.Fatal("expected the no-DID error")
	}
	if !strings.Contains(err.Error(), "DID") {
		t.Errorf("error = %q, want it to name the missing DID", err)
	}
}

// A CRLF-authored .env must not leave \r on a base64 signing key: it fails
// signature verification with an error naming neither the file nor the character.
func TestResolveCredentials_CRLFStrippedFromCredentialFile(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvDID, testDID)
	path, err := EnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := EnvAPIKeyDirect + "=obx_k\r\n" + EnvAgentPrivateKey + "=YmFzZTY0\r\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.PrivateKeyB64 != "YmFzZTY0" {
		t.Errorf("private key = %q, want no trailing carriage return", c.PrivateKeyB64)
	}
}

// The deprecated names keep working so nobody's CI breaks on upgrade. Three names
// existed for one value and only OPENBOX_AGENT_PRIVATE_KEY was ever documented.
func TestResolveCredentials_DeprecatedPrivateKeyNames(t *testing.T) {
	for _, alias := range deprecatedPrivateKeyEnvNames {
		t.Run(alias+"/env", func(t *testing.T) {
			isolateConfig(t)
			t.Setenv(EnvDID, testDID)
			t.Setenv(EnvAPIKeyDirect, "obx_k")
			t.Setenv(alias, "YWxpYXM=")
			c, err := ResolveCredentials()
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if c.PrivateKeyB64 != "YWxpYXM=" {
				t.Errorf("private key = %q, want the deprecated %s to still be read", c.PrivateKeyB64, alias)
			}
		})
		t.Run(alias+"/file", func(t *testing.T) {
			isolateConfig(t)
			t.Setenv(EnvDID, testDID)
			writeEnvFileForTest(t, map[string]string{EnvAPIKeyDirect: "obx_k", alias: "YWxpYXM="})
			c, err := ResolveCredentials()
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if c.PrivateKeyB64 != "YWxpYXM=" {
				t.Errorf("private key = %q, want the deprecated %s to still be read from the file", c.PrivateKeyB64, alias)
			}
		})
	}
}

// The documented name wins over a deprecated alias when both are set.
func TestResolveCredentials_DocumentedNameBeatsAlias(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvDID, testDID)
	t.Setenv(EnvAPIKeyDirect, "obx_k")
	t.Setenv(EnvAgentPrivateKey, "Y3VycmVudA==")
	t.Setenv("OPENBOX_ED25519_SEED", "b2xk")
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.PrivateKeyB64 != "Y3VycmVudA==" {
		t.Errorf("private key = %q, want the documented name to win", c.PrivateKeyB64)
	}
}

func TestResolveCredentials_MissingSecret(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvDID, testDID)
	_, err := ResolveCredentials()
	if err == nil {
		t.Fatal("expected an error when no api key source is configured")
	}
	// The error is where a user upgrading an existing install meets the
	// no-keychain-migration decision, so it must name the way out.
	if !strings.Contains(err.Error(), "openbox auth") {
		t.Errorf("error = %q, want it to name `openbox auth`", err)
	}
}

// An unparseable credential file must not read as "no credentials": that would
// send a user hunting for a registration problem they do not have.
func TestResolveCredentials_UnparseableFileIsAnError(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvDID, testDID)
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

func TestResolveContentCapture_DefaultOn(t *testing.T) {
	// DEFAULT ON (brian 2026-07-15): absent config field + no env → enabled.
	isolateConfig(t)
	if !ResolveContentCapture() {
		t.Error("ResolveContentCapture default must be ON (absent config)")
	}
	// Explicit config opt-out honored (absent vs explicit-false via *bool).
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	_ = os.WriteFile(cfgPath, []byte(`{"developer_did":"`+testDID+`","content_capture":false}`), 0o600)
	t.Setenv(EnvConfigPath, cfgPath)
	if ResolveContentCapture() {
		t.Error("explicit content_capture:false must opt out")
	}
	// Env wins over both.
	t.Setenv(EnvContentCapture, "0")
	if ResolveContentCapture() {
		t.Error("OPENBOX_CONTENT_CAPTURE=0 must force OFF")
	}
}

// TestResolveFinops_DefaultOn pins the ADR-0014 posture flip, and it pins the
// ABSENT-FIELD case specifically — which is the case the old implementation could
// not express. `Finops` was a plain bool whose resolver returned `&b`
// unconditionally, so resolveBool never reached its default: an absent `finops`
// key and an explicit `finops:false` were the same input, and moving the default
// would have changed nothing for every config file that already existed while
// pinning every absent field to false forever.
//
// So the first assertion here is not a formality. It is the one that would have
// failed before `Finops` became a *bool, and the reason a future flip in either
// direction has to be a deliberate edit to this test.
func TestResolveFinops_DefaultOn(t *testing.T) {
	// Absent config field + no env → ON. Usage capture is an egress posture, so
	// this is the assertion that says "an unconfigured developer machine sends
	// token counts and a model id" out loud.
	isolateConfig(t)
	if !ResolveFinops() {
		t.Error("ResolveFinops default must be ON (absent config field)")
	}

	// A config file that exists but says nothing about finops is still the absent
	// case — the distinction the *bool exists to draw.
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	_ = os.WriteFile(cfgPath, []byte(`{"developer_did":"`+testDID+`"}`), 0o600)
	t.Setenv(EnvConfigPath, cfgPath)
	if !ResolveFinops() {
		t.Error("a config file with no finops key must resolve to the default (ON), not to false")
	}

	// Explicit managed-config opt-out.
	_ = os.WriteFile(cfgPath, []byte(`{"developer_did":"`+testDID+`","finops":false}`), 0o600)
	if ResolveFinops() {
		t.Error("explicit finops:false must opt out")
	}

	// Env wins over config, in both directions.
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

// The posture record must report the SAME state the resolver does, or the
// evidence an auditor reads contradicts what the session actually did — which is
// worse than having no evidence, because it is trusted.
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
	// And the flag is reported in the generic map the endpoint/doctor read, not
	// only on the struct.
	if got, ok := p.Flags()["finops"]; !ok || got {
		t.Errorf("Flags()[finops] = (%t, present=%t), want (false, true)", got, ok)
	}

	t.Setenv(EnvFinops, "1")
	if !EffectivePosture().Finops {
		t.Error("posture must follow the env override")
	}
}

func TestResolveRealtime_DefaultOn(t *testing.T) {
	// Default ON: absent config field + no env → real-time delivery enabled.
	isolateConfig(t)
	if !ResolveRealtime() {
		t.Error("ResolveRealtime default must be ON (absent config)")
	}
	// Explicit config opt-out honored (absent vs explicit-false via *bool).
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	_ = os.WriteFile(cfgPath, []byte(`{"developer_did":"`+testDID+`","realtime_flush":false}`), 0o600)
	t.Setenv(EnvConfigPath, cfgPath)
	if ResolveRealtime() {
		t.Error("explicit realtime_flush:false must opt out")
	}
	// Env wins over config in both directions.
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

	// Default: no config field, no env → TRUE (ADR-0016 reversed the observe
	// default). Safe because enforcement is inert without an org policy and
	// fail_closed stays off, so an outage never blocks a tool call.
	write(`{"developer_did":"` + testDID + `"}`)
	if !ResolveEnforce() {
		t.Error("an absent enforce field must resolve to ON (ADR-0016)")
	}
	// And an explicit false must still be honoured, which is the property the
	// *bool type change bought: as a plain bool it marshalled to nothing.
	write(`{"developer_did":"` + testDID + `","enforce":false}`)
	if ResolveEnforce() {
		t.Error("enforce:false in config must opt out")
	}
	// Config enables it.
	write(`{"developer_did":"` + testDID + `","enforce":true}`)
	if !ResolveEnforce() {
		t.Error("enforce:true in config should enable")
	}
	// Env overrides config either way.
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

	// Absent everywhere → 0 (caller substitutes its default).
	_ = os.WriteFile(cfgPath, []byte(`{}`), 0o600)
	if ms := ResolveTimeoutMS(field, EnvTier2Timeout); ms != 0 {
		t.Errorf("unset = %d, want 0", ms)
	}
	// Config value honored.
	_ = os.WriteFile(cfgPath, []byte(`{"tier2_timeout_ms":250}`), 0o600)
	if ms := ResolveTimeoutMS(field, EnvTier2Timeout); ms != 250 {
		t.Errorf("config = %d, want 250", ms)
	}
	// Env overrides config.
	t.Setenv(EnvTier2Timeout, "500")
	if ms := ResolveTimeoutMS(field, EnvTier2Timeout); ms != 500 {
		t.Errorf("env = %d, want 500", ms)
	}
	// Unparseable env is ignored (config stands).
	t.Setenv(EnvTier2Timeout, "not-a-number")
	if ms := ResolveTimeoutMS(field, EnvTier2Timeout); ms != 250 {
		t.Errorf("garbage env should fall back to config 250, got %d", ms)
	}
}

// The arg-injection guard this used to test is gone with its subject: credential
// resolution no longer shells out to `security`/`secret-tool` with coordinates
// taken from config, so there is no argv for a crafted value to be reparsed into
// (ADR-0015). One fewer attack surface, not one fewer check.

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
//
// `omitempty` drops a plain `false`, so an org's deliberate `telemetry:false`
// would vanish from the file the next time anything rewrote it — and since the
// default is ON, the lane would silently switch itself back on. That is the bug
// ADR-0016 records for `Enforce`, and the reason every posture key here is a
// pointer. Drilled 2026-08-28: as a plain bool this marshals to `{}`.
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

	// And absent must stay absent, or every config file grows keys nobody set.
	bare, err := json.Marshal(DevConfig{})
	if err != nil {
		t.Fatalf("marshal bare: %v", err)
	}
	if strings.Contains(string(bare), "telemetry") {
		t.Errorf("an unset field was written anyway: %s", bare)
	}
}

// TestResolveTelemetryDefaultsOnAndEnvWins pins the posture resolution.
//
// Default ON is ADR-0016's ResolveFinops lesson: INSTALLING the lane is the
// opt-in, so a default-off second switch would leave a developer who ran the
// install command with a daemon that receives everything and records nothing.
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
