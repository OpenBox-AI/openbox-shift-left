package claudecode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
)

// TestResolveCredentials_FromCredentialFile proves the hook reads
// ~/.openbox/.env, which is where `openbox auth` writes credentials (ADR-0015)
// and the position the deleted OS secret store used to hold.
func TestResolveCredentials_FromCredentialFile(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	writeCredentialFile(t, map[string]string{
		envAPIKeyDirect:    "obx_from_file",
		envAgentPrivateKey: "c2VlZGZyb21maWxl",
	})

	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.APIKey != "obx_from_file" || c.PrivateKeyB64 != "c2VlZGZyb21maWxl" {
		t.Errorf("creds from credential file = %+v", c)
	}
}

// isolateConfig points the dev config AND the credential file at empty temp
// locations, so no test reads the developer machine's real ~/.openbox.
//
// This replaced an injectable secretLookup seam: with credentials in a plaintext
// file, a test can drive the same code path production does instead of
// substituting a function for it (ADR-0015).
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv(envConfigPath, filepath.Join(t.TempDir(), "none.json"))
	t.Setenv(devconfig.EnvHome, t.TempDir())

	// OPENBOX_HOME does NOT cover these three. They resolve from
	// os.UserConfigDir() instead, and that split is deliberate (see
	// devconfig/paths.go) — so pinning OPENBOX_HOME alone left every test that
	// reached the enforce path appending to the DEVELOPER'S real audit sink and
	// filing real approval markers. Each has its own documented override; this
	// is the one place that has to remember all of them.
	// Never clobber a pin the test already made: helpers like findingsEnv point
	// the advisory sink at a file they then seed, and they are called either side
	// of this one. Requiring a call order instead would be the same fragility
	// that let the escape happen — one that only shows up as a confusing
	// assertion failure, or as a silent write to the real sink.
	sinks := t.TempDir()
	pinIfUnset := func(name, path string) {
		if os.Getenv(name) == "" {
			t.Setenv(name, path)
		}
	}
	pinIfUnset(devconfig.EnvEnforcementFile, filepath.Join(sinks, "enforcements.jsonl"))
	pinIfUnset(devconfig.EnvPendingApprovalDir, filepath.Join(sinks, "pending-approvals"))
	pinIfUnset("OPENBOX_ADVISORY_FILE", filepath.Join(sinks, "advisories.jsonl"))

	for _, name := range []string{envAPIKeyDirect, envAgentPrivateKey, "OPENBOX_ED25519_SEED", "OPENBOX_SEED"} {
		t.Setenv(name, "")
	}
}

// writeCredentialFile writes ~/.openbox/.env under the isolated OPENBOX_HOME.
func writeCredentialFile(t *testing.T, kv map[string]string) {
	t.Helper()
	path, err := devconfig.EnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteEnvFile(path, kv); err != nil {
		t.Fatal(err)
	}
}

func TestResolveIdentity(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	id, err := ResolveIdentity()
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if id.DeveloperDID != testDID {
		t.Errorf("DID = %q, want %q", id.DeveloperDID, testDID)
	}
}

func TestResolveIdentityMissing(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, "")
	if _, err := ResolveIdentity(); err == nil {
		t.Error("expected error when no DID configured")
	}
}

func TestResolveIdentityFromConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dev.json")
	if err := os.WriteFile(cfgPath, []byte(`{"developer_did":"`+testDID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envConfigPath, cfgPath)
	t.Setenv(envDID, "") // no env override → must come from the file
	id, err := ResolveIdentity()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id.DeveloperDID != testDID {
		t.Errorf("DID from config = %q, want %q", id.DeveloperDID, testDID)
	}
}

func TestResolveCredentials_DirectEnvOverride(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv(envAPIKeyDirect, "obx_test_key")
	t.Setenv(envAgentPrivateKey, "c2VlZA==")
	t.Setenv(envBaseURL, "https://core.example.ai")
	t.Setenv(envContentCapture, "true")

	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.APIKey != "obx_test_key" || c.PrivateKeyB64 != "c2VlZA==" {
		t.Errorf("creds = %+v", c)
	}
	if c.BaseURL != "https://core.example.ai" {
		t.Errorf("base url = %q", c.BaseURL)
	}
	if !c.ContentCaptureEnabled {
		t.Error("content capture should be enabled")
	}
	if c.Identity().DeveloperDID != testDID {
		t.Errorf("identity DID = %q", c.Identity().DeveloperDID)
	}
}

// A real environment variable beats the credential file, so CI can override
// without writing to disk.
func TestResolveCredentials_RealEnvBeatsCredentialFile(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	writeCredentialFile(t, map[string]string{
		envAPIKeyDirect:    "obx_from_file",
		envAgentPrivateKey: "ZmlsZQ==",
	})
	t.Setenv(envAPIKeyDirect, "obx_from_env")
	t.Setenv(envAgentPrivateKey, "ZW52")

	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.APIKey != "obx_from_env" || c.PrivateKeyB64 != "ZW52" {
		t.Errorf("creds = %+v, want the environment to win", c)
	}
	if c.BaseURL != defaultBaseURL {
		t.Errorf("base url default = %q, want %q", c.BaseURL, defaultBaseURL)
	}
}

func TestResolveCredentials_EnvDisablesConfigContentCapture(t *testing.T) {
	// Config enables content capture; env must be able to turn it back OFF (F7).
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dev.json")
	_ = os.WriteFile(cfgPath, []byte(`{"developer_did":"`+testDID+`","content_capture":true}`), 0o600)
	t.Setenv(envConfigPath, cfgPath)
	t.Setenv(envDID, "")
	t.Setenv(envAPIKeyDirect, "obx_k")
	t.Setenv(envAgentPrivateKey, "c2VlZA==")
	t.Setenv(envContentCapture, "false")

	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.ContentCaptureEnabled {
		t.Error("env content_capture=false must override config's true (env-overrides-config)")
	}
}

func TestResolveContentCapture_DefaultOn(t *testing.T) {
	// DEFAULT ON (brian 2026-07-15): an absent config field + no env → content
	// capture is ENABLED. An explicit config false / env 0 opts back out.
	isolateConfig(t) // empty config, no OPENBOX_CONTENT_CAPTURE
	if !ResolveContentCapture() {
		t.Error("ResolveContentCapture default must be ON (absent config)")
	}
	// ResolveCredentials derives the same default.
	t.Setenv(envDID, testDID)
	t.Setenv(envAPIKeyDirect, "obx_k")
	t.Setenv(envAgentPrivateKey, "c2VlZA==")
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !c.ContentCaptureEnabled {
		t.Error("ResolveCredentials ContentCaptureEnabled default must be ON")
	}
	// Explicit config opt-out is honored (absent vs explicit-false distinguished via *bool).
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	_ = os.WriteFile(cfgPath, []byte(`{"developer_did":"`+testDID+`","content_capture":false}`), 0o600)
	t.Setenv(envConfigPath, cfgPath)
	if ResolveContentCapture() {
		t.Error("explicit content_capture:false must opt out")
	}
	// Env wins over both.
	t.Setenv(envContentCapture, "0")
	if ResolveContentCapture() {
		t.Error("OPENBOX_CONTENT_CAPTURE=0 must force OFF")
	}
}

func TestResolveInstallGitHook(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dev.json")
	write := func(json string) { _ = os.WriteFile(cfgPath, []byte(json), 0o600) }
	t.Setenv(envConfigPath, cfgPath)
	os.Unsetenv(envInstallGitHook) // env genuinely absent → config decides

	// Default: no config field, no env → false (fail-safe; does not modify repos).
	write(`{"developer_did":"` + testDID + `"}`)
	if ResolveInstallGitHook() {
		t.Error("default should be false")
	}

	// Config enables it.
	write(`{"developer_did":"` + testDID + `","install_git_hook":true}`)
	if !ResolveInstallGitHook() {
		t.Error("install_git_hook:true in config should enable")
	}

	// Env overrides config either way.
	t.Setenv(envInstallGitHook, "false")
	if ResolveInstallGitHook() {
		t.Error("env false must override config true")
	}
	write(`{"developer_did":"` + testDID + `"}`)
	t.Setenv(envInstallGitHook, "1")
	if !ResolveInstallGitHook() {
		t.Error("env 1 must override config absent/false")
	}
}

// TestResolveFailClosed guards E6-S3 AC-1: fail-open is the default (an org never
// becomes fail-closed by accident); config enables it; the env overrides either way.
func TestResolveFailClosed(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	write := func(json string) { _ = os.WriteFile(cfgPath, []byte(json), 0o600) }
	t.Setenv(envConfigPath, cfgPath)
	os.Unsetenv(envFailClosed) // env genuinely absent → config decides

	// Default: no config field, no env → false (fail-OPEN, OD9).
	write(`{"developer_did":"` + testDID + `"}`)
	if ResolveFailClosed() {
		t.Error("default must be fail-open (false) — never fail-closed by accident")
	}

	// Config opts into fail-closed.
	write(`{"developer_did":"` + testDID + `","fail_closed":true}`)
	if !ResolveFailClosed() {
		t.Error("fail_closed:true in config should enable fail-closed")
	}

	// Env overrides config either way.
	t.Setenv(envFailClosed, "false")
	if ResolveFailClosed() {
		t.Error("env false must override config true")
	}
	write(`{"developer_did":"` + testDID + `"}`)
	t.Setenv(envFailClosed, "1")
	if !ResolveFailClosed() {
		t.Error("env 1 must override config absent/false")
	}
}

func TestResolveCredentials_MissingSecret(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	// No API key source at all → error (fail-open at the caller, but resolve errs).
	if _, err := ResolveCredentials(); err == nil {
		t.Error("expected error when no api key source is configured")
	}
}
