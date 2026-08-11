package devconfig

import (
	"os"
	"path/filepath"
	"testing"
)

const testDID = "did:aip:7f3c9b2e-0000-5000-a000-000000000001"

// isolateConfig points OPENBOX_CONFIG at a nonexistent temp path so tests never
// read the developer machine's real ~/.config/openbox/dev.json.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv(EnvConfigPath, filepath.Join(t.TempDir(), "none.json"))
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

func TestResolveCredentials_DirectEnvOverride(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvDID, testDID)
	t.Setenv(EnvAPIKeyDirect, "obx_test_key")
	t.Setenv(EnvSeedDirect, "c2VlZA==")
	t.Setenv(EnvBaseURL, "https://core.example.ai")
	t.Setenv(EnvContentCapture, "true")

	c, err := ResolveCredentials(nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.APIKey != "obx_test_key" || c.SeedB64 != "c2VlZA==" {
		t.Errorf("creds = %+v", c)
	}
	if c.BaseURL != "https://core.example.ai" {
		t.Errorf("base url = %q", c.BaseURL)
	}
	if !c.ContentCaptureEnabled {
		t.Error("content capture should be enabled")
	}
}

func TestResolveCredentials_InjectedLookup(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvDID, testDID)
	t.Setenv(EnvSecretService, "openbox-dev")
	t.Setenv(EnvAPIKeyAccount, "apikey")
	t.Setenv(EnvPrivKeyAccount, "seed")

	lookup := func(service, account string) (string, error) {
		if service != "openbox-dev" {
			t.Errorf("unexpected service %q", service)
		}
		switch account {
		case "apikey":
			return "obx_from_store", nil
		case "seed":
			return "c2VlZGZyb21zdG9yZQ==", nil
		}
		return "", os.ErrNotExist
	}

	c, err := ResolveCredentials(lookup)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.APIKey != "obx_from_store" || c.SeedB64 != "c2VlZGZyb21zdG9yZQ==" {
		t.Errorf("creds from store = %+v", c)
	}
	if c.BaseURL != DefaultBaseURL {
		t.Errorf("base url default = %q, want %q", c.BaseURL, DefaultBaseURL)
	}
}

// The file secret backend (OPENBOX_SECRET_FILE) overrides the injected lookup —
// the same nested-JSON format the CLI's fileStore writes.
func TestResolveCredentials_FileBackend(t *testing.T) {
	isolateConfig(t)
	path := filepath.Join(t.TempDir(), "secrets.json")
	blob := `{"ai.openbox.dev":{"acme/codex/api_key":"obx_from_file","acme/codex/private_key":"c2VlZGZyb21maWxl"}}`
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvDID, testDID)
	t.Setenv(EnvSecretFile, path)
	t.Setenv(EnvSecretService, "ai.openbox.dev")
	t.Setenv(EnvAPIKeyAccount, "acme/codex/api_key")
	t.Setenv(EnvPrivKeyAccount, "acme/codex/private_key")

	c, err := ResolveCredentials(func(string, string) (string, error) {
		t.Fatal("OS lookup must not be consulted when a secret file is configured")
		return "", nil
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.APIKey != "obx_from_file" || c.SeedB64 != "c2VlZGZyb21maWxl" {
		t.Errorf("creds from file backend = %+v", c)
	}
}

func TestResolveCredentials_MissingSecret(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvDID, testDID)
	t.Setenv(EnvAPIKeyDirect, "")
	t.Setenv(EnvSeedDirect, "")
	t.Setenv(EnvSecretService, "")
	if _, err := ResolveCredentials(nil); err == nil {
		t.Error("expected error when no api key source is configured")
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

	// Default: no config field, no env → false (observe-only, fail-safe).
	write(`{"developer_did":"` + testDID + `"}`)
	if ResolveEnforce() {
		t.Error("default should be false")
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

func TestOSSecretLookup_RejectsDashCoordinates(t *testing.T) {
	if _, err := OSSecretLookup("-flag", "acct"); err == nil {
		t.Error("leading-dash service should be rejected (arg-injection guard)")
	}
	if _, err := OSSecretLookup("svc", "-flag"); err == nil {
		t.Error("leading-dash account should be rejected")
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
