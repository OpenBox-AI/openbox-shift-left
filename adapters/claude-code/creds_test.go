package claudecode

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestResolveCredentials_FileBackend proves the hook reads the CLI's opt-in
// file secret backend (`dev init --secret-backend file`) when OPENBOX_SECRET_FILE
// points at it — the fix for machines with no OS keyring.
func TestResolveCredentials_FileBackend(t *testing.T) {
	isolateConfig(t)
	path := filepath.Join(t.TempDir(), "secrets.json")
	// Same nested-JSON format the CLI's fileStore writes.
	blob := `{"ai.openbox.dev":{"acme/claude-code/api_key":"obx_from_file","acme/claude-code/private_key":"c2VlZGZyb21maWxl"}}`
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envDID, testDID)
	t.Setenv(envSecretFile, path)
	t.Setenv(envSecretService, "ai.openbox.dev")
	t.Setenv(envAPIKeyAccount, "acme/claude-code/api_key")
	t.Setenv(envPrivKeyAccount, "acme/claude-code/private_key")

	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.APIKey != "obx_from_file" || c.SeedB64 != "c2VlZGZyb21maWxl" {
		t.Errorf("creds from file backend = %+v", c)
	}
}

// isolateConfig points OPENBOX_CONFIG at a nonexistent temp path so tests never
// read the developer machine's real ~/.config/openbox/dev.json.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv(envConfigPath, filepath.Join(t.TempDir(), "none.json"))
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
	t.Setenv(envSeedDirect, "c2VlZA==")
	t.Setenv(envBaseURL, "https://core.example.ai")
	t.Setenv(envContentCapture, "true")

	c, err := ResolveCredentials()
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
	if c.Identity().DeveloperDID != testDID {
		t.Errorf("identity DID = %q", c.Identity().DeveloperDID)
	}
}

func TestResolveCredentials_SecretStore(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv(envSecretService, "openbox-dev")
	t.Setenv(envAPIKeyAccount, "apikey")
	t.Setenv(envPrivKeyAccount, "seed")

	// Inject a fake secret store.
	orig := secretLookup
	t.Cleanup(func() { secretLookup = orig })
	secretLookup = func(service, account string) (string, error) {
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

	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.APIKey != "obx_from_store" || c.SeedB64 != "c2VlZGZyb21zdG9yZQ==" {
		t.Errorf("creds from store = %+v", c)
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
	t.Setenv(envSeedDirect, "c2VlZA==")
	t.Setenv(envContentCapture, "false")

	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.ContentCaptureEnabled {
		t.Error("env content_capture=false must override config's true (env-overrides-config)")
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

// TestResolveEnforceTimeout guards E6-S3 AC-5: default 0 (⇒ sidecar default), env
// wins over config, and a value over maxEnforceTimeout is clamped so the wait
// stays under Claude Code's 5 s hook kill.
func TestResolveEnforceTimeout(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	write := func(json string) { _ = os.WriteFile(cfgPath, []byte(json), 0o600) }
	t.Setenv(envConfigPath, cfgPath)
	os.Unsetenv(envEnforceTimeout)

	// Default: unset → 0 (caller lets sidecar.NewClient use DefaultDecisionTimeout).
	write(`{"developer_did":"` + testDID + `"}`)
	if d := ResolveEnforceTimeout(); d != 0 {
		t.Errorf("unset timeout = %v, want 0 (⇒ sidecar default)", d)
	}

	// Config value honored.
	write(`{"developer_did":"` + testDID + `","enforce_timeout_ms":250}`)
	if d := ResolveEnforceTimeout(); d != 250*time.Millisecond {
		t.Errorf("config timeout = %v, want 250ms", d)
	}

	// Env overrides config.
	t.Setenv(envEnforceTimeout, "500")
	if d := ResolveEnforceTimeout(); d != 500*time.Millisecond {
		t.Errorf("env timeout = %v, want 500ms", d)
	}

	// Non-positive / unparseable env → 0 (fall back to the sidecar default).
	t.Setenv(envEnforceTimeout, "0")
	if d := ResolveEnforceTimeout(); d != 0 {
		t.Errorf("zero env timeout = %v, want 0", d)
	}
	t.Setenv(envEnforceTimeout, "not-a-number")
	if d := ResolveEnforceTimeout(); d != 250*time.Millisecond {
		t.Errorf("unparseable env should fall back to config 250ms, got %v", d)
	}

	// Negative env → 0 (<=0 falls back to the sidecar default).
	t.Setenv(envEnforceTimeout, "-1")
	if d := ResolveEnforceTimeout(); d != 0 {
		t.Errorf("negative env timeout = %v, want 0", d)
	}

	// Over the ceiling → clamped (INV-3b bounded; stays under CC's 5 s kill).
	t.Setenv(envEnforceTimeout, "600000") // 600 s
	if d := ResolveEnforceTimeout(); d != maxEnforceTimeout {
		t.Errorf("oversized timeout = %v, want clamp to %v", d, maxEnforceTimeout)
	}

	// Near-max-int64 must NOT overflow into a negative/huge duration — clamp holds.
	t.Setenv(envEnforceTimeout, "9223372036854775807")
	if d := ResolveEnforceTimeout(); d != maxEnforceTimeout {
		t.Errorf("overflow-range timeout = %v, want clamp to %v", d, maxEnforceTimeout)
	}
}

func TestOsSecretLookup_RejectsDashCoordinates(t *testing.T) {
	if _, err := osSecretLookup("-flag", "acct"); err == nil {
		t.Error("leading-dash service should be rejected (arg-injection guard)")
	}
	if _, err := osSecretLookup("svc", "-flag"); err == nil {
		t.Error("leading-dash account should be rejected")
	}
}

func TestResolveCredentials_MissingSecret(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	// No API key source at all → error (fail-open at the caller, but resolve errs).
	t.Setenv(envAPIKeyDirect, "")
	t.Setenv(envSeedDirect, "")
	t.Setenv(envSecretService, "")
	if _, err := ResolveCredentials(); err == nil {
		t.Error("expected error when no api key source is configured")
	}
}
