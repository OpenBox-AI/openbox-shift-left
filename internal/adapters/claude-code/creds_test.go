package claudecode

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// TestLegacyStoreHookSendsNothing a stored developer_did means the store
// predates v3 entirely (D1): ResolveIdentity refuses with ErrLegacyStore, the
// mapper never even sees the event, and the hook stays exactly as silent as
// it is for a genuinely unconfigured machine -- an unmapped event, an empty
// spool, no request.
func TestLegacyStoreHookSendsNothing(t *testing.T) {
	isolateConfig(t)
	spool := t.TempDir()
	t.Setenv("OPENBOX_SPOOL_DIR", spool)
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	if err := devconfig.WriteConfig(DefaultConfigPath(), devconfig.Update{DID: "did:aip:legacy-store"}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	var errb bytes.Buffer
	RunHook("PreToolUse", strings.NewReader(
		`{"hook_event_name":"PreToolUse","session_id":"s1","cwd":"/r","tool_name":"Bash","tool_input":{"command":"ls"}}`),
		&out, log.New(&errb, "", 0))

	if out.Len() != 0 {
		t.Fatalf("stdout must stay empty, got %q", out.String())
	}
	if !strings.Contains(errb.String(), "no identity") {
		t.Errorf("expected a no-identity drop notice, got %q", errb.String())
	}
	entries, _ := os.ReadDir(spool)
	if len(entries) != 0 {
		t.Errorf("a legacy store's event reached the spool: %v", entries)
	}
}

// TestResolveCredentials_FromCredentialFile proves the hook reads
// ~/.openbox/<tool>/.env, which is where `openbox init` writes credentials and the
// position the deleted OS secret store used to hold. The agent id, a
// coordinate, comes from dev.json, never from the credential file.
func TestResolveCredentials_FromCredentialFile(t *testing.T) {
	isolateConfig(t)
	writeDevConfigAgentID(t, testAgentID)
	writeCredentialFile(t, map[string]string{
		envAPIKeyDirect:       "obx_from_file",
		envWorkloadPrivateKey: "wk_from_file",
	})

	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.APIKey != "obx_from_file" || c.WorkloadPrivateKey != "wk_from_file" {
		t.Errorf("creds from credential file = %+v", c)
	}
}

func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv(envConfigPath, filepath.Join(t.TempDir(), "none.json"))
	t.Setenv(devconfig.EnvHome, t.TempDir())

	// Every production caller of this adapter runs under `openbox hook
	// claude-code`, which binds before anything resolves a credential. Leaving
	// the fixture unbound would read the org store, which under the per-tool
	// split holds no agent identity at all.
	release, err := devconfig.BindProvider("claude-code")
	if err != nil {
		t.Fatalf("bind claude-code: %v", err)
	}
	t.Cleanup(release)

	// Never clobber a pin the test already made: helpers like findingsEnv point
	// the advisory sink at a file they then seed, and they are called either side
	// of this one.
	sinks := t.TempDir()
	pinIfUnset := func(name, path string) {
		if os.Getenv(name) == "" {
			t.Setenv(name, path)
		}
	}
	pinIfUnset(devconfig.EnvEnforcementFile, filepath.Join(sinks, "enforcements.jsonl"))
	pinIfUnset(devconfig.EnvPendingApprovalDir, filepath.Join(sinks, "pending-approvals"))
	pinIfUnset("OPENBOX_ADVISORY_FILE", filepath.Join(sinks, "advisories.jsonl"))

	for _, name := range append([]string{envAPIKeyDirect, envWorkloadPrivateKey, envAgentID, envDID, envAgentPrivateKey},
		"OPENBOX_ED25519_SEED", "OPENBOX_SEED") {
		t.Setenv(name, "")
	}
}

// writeDevConfigAgentID writes the bound tool's dev.json with only agent_id
// set, honouring the OPENBOX_CONFIG pin exactly the way ResolveCredentials
// itself resolves the path.
func writeDevConfigAgentID(t *testing.T, agentID string) {
	t.Helper()
	if err := devconfig.WriteConfig(DefaultConfigPath(), devconfig.Update{AgentID: agentID}); err != nil {
		t.Fatal(err)
	}
}

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
	t.Setenv(envAgentID, testAgentID)
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
	t.Setenv(envAgentID, "")
	if _, err := ResolveIdentity(); err == nil {
		t.Error("expected error when no agent id configured")
	}
}

func TestResolveIdentityFromConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dev.json")
	if err := os.WriteFile(cfgPath, []byte(`{"agent_id":"`+testAgentID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envConfigPath, cfgPath)
	t.Setenv(envAgentID, "") // no env override → must come from the file
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
	t.Setenv(envAgentID, testAgentID)
	t.Setenv(envAPIKeyDirect, "obx_test_key")
	t.Setenv(envWorkloadPrivateKey, "wk_test_key")
	t.Setenv(envBaseURL, "https://core.example.ai")
	t.Setenv(envContentCapture, "true")

	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.APIKey != "obx_test_key" || c.WorkloadPrivateKey != "wk_test_key" {
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

// TestResolveCredentials_RealEnvBeatsCredentialFile a real environment
// variable beats the credential file, so CI can override without writing to
// disk.
func TestResolveCredentials_RealEnvBeatsCredentialFile(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envAgentID, testAgentID)
	writeCredentialFile(t, map[string]string{
		envAPIKeyDirect:       "obx_from_file",
		envWorkloadPrivateKey: "wk_from_file",
	})
	t.Setenv(envAPIKeyDirect, "obx_from_env")
	t.Setenv(envWorkloadPrivateKey, "wk_from_env")

	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.APIKey != "obx_from_env" || c.WorkloadPrivateKey != "wk_from_env" {
		t.Errorf("creds = %+v, want the environment to win", c)
	}
	if c.BaseURL != defaultBaseURL {
		t.Errorf("base url default = %q, want %q", c.BaseURL, defaultBaseURL)
	}
}

func TestResolveCredentials_EnvDisablesConfigContentCapture(t *testing.T) {
	isolateConfig(t) // binds: a hook resolves its credentials under its own tool
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dev.json")
	_ = os.WriteFile(cfgPath, []byte(`{"agent_id":"`+testAgentID+`","content_capture":true}`), 0o600)
	t.Setenv(envConfigPath, cfgPath)
	t.Setenv(envAgentID, "")
	t.Setenv(envAPIKeyDirect, "obx_k")
	t.Setenv(envWorkloadPrivateKey, "wk_k")
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
	isolateConfig(t) // empty config, no OPENBOX_CONTENT_CAPTURE
	if !ResolveContentCapture() {
		t.Error("ResolveContentCapture default must be ON (absent config)")
	}
	t.Setenv(envAgentID, testAgentID)
	t.Setenv(envAPIKeyDirect, "obx_k")
	t.Setenv(envWorkloadPrivateKey, "wk_k")
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !c.ContentCaptureEnabled {
		t.Error("ResolveCredentials ContentCaptureEnabled default must be ON")
	}
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	_ = os.WriteFile(cfgPath, []byte(`{"agent_id":"`+testAgentID+`","content_capture":false}`), 0o600)
	t.Setenv(envConfigPath, cfgPath)
	if ResolveContentCapture() {
		t.Error("explicit content_capture:false must opt out")
	}
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

	write(`{"developer_did":"` + testDID + `"}`)
	if ResolveInstallGitHook() {
		t.Error("default should be false")
	}

	write(`{"developer_did":"` + testDID + `","install_git_hook":true}`)
	if !ResolveInstallGitHook() {
		t.Error("install_git_hook:true in config should enable")
	}

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

// TestResolveFailClosed guards E6-S3 AC-1: fail-open is the default (an org
// never becomes fail-closed by accident); config enables it; the env overrides
// either way.
func TestResolveFailClosed(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	write := func(json string) { _ = os.WriteFile(cfgPath, []byte(json), 0o600) }
	t.Setenv(envConfigPath, cfgPath)
	os.Unsetenv(envFailClosed) // env genuinely absent → config decides

	write(`{"developer_did":"` + testDID + `"}`)
	if ResolveFailClosed() {
		t.Error("default must be fail-open (false); never fail-closed by accident")
	}

	write(`{"developer_did":"` + testDID + `","fail_closed":true}`)
	if !ResolveFailClosed() {
		t.Error("fail_closed:true in config should enable fail-closed")
	}

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
	t.Setenv(envAgentID, testAgentID)
	if _, err := ResolveCredentials(); err == nil {
		t.Error("expected error when no api key source is configured")
	}
}
