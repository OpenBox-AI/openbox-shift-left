package devconfig

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteWorkloadIdentityReplacesALegacyStore seeds a legacy dev.json + .env
// (a stored DID, an Ed25519 seed, a stale token cache), writes a v3 identity
// over it, and asserts every legacy trace is gone and every v3 field is
// present. It then runs the exact same write again and asserts the config and
// credential files come out byte-identical, the second-invocation rule every
// write path in this package is held to.
func TestWriteWorkloadIdentityReplacesALegacyStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvHome, home)
	t.Setenv(EnvConfigPath, "")

	cfgPath, err := DevConfigWritePathFor("codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteConfig(cfgPath, Update{BaseURL: "https://core.example"}); err != nil {
		t.Fatal(err)
	}
	if err := SetLegacyDID(cfgPath, "did:aip:legacy"); err != nil {
		t.Fatal(err)
	}
	envPath, err := EnvFilePathFor("codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteEnvFile(envPath, map[string]string{
		EnvAPIKeyDirect:    "obx_legacy",
		EnvAgentPrivateKey: "bGVnYWN5c2VlZA==",
	}); err != nil {
		t.Fatal(err)
	}
	cachePath, err := WorkloadTokenCachePathFor("codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte(`{"access_token":"stale"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	id := WorkloadIdentity{AgentID: "agent-v3", APIKey: "obx_v3", PrivateKeyB64: "dGhlLXYzLWtleQ=="}
	if err := WriteWorkloadIdentity("codex", id); err != nil {
		t.Fatalf("WriteWorkloadIdentity: %v", err)
	}

	cfg := mustLoad(t, cfgPath)
	if cfg.DID != "" {
		t.Errorf("DID = %q, want cleared", cfg.DID)
	}
	if cfg.IdentityMethod != IdentityMethodKeycloakWorkload {
		t.Errorf("identity_method = %q, want %q", cfg.IdentityMethod, IdentityMethodKeycloakWorkload)
	}
	if cfg.AgentID != "agent-v3" {
		t.Errorf("agent_id = %q, want agent-v3", cfg.AgentID)
	}
	if cfg.BaseURL != "https://core.example" {
		t.Errorf("base_url = %q, want the untouched coordinate preserved", cfg.BaseURL)
	}

	secrets, err := ParseEnvFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if secrets[EnvAPIKeyDirect] != "obx_v3" {
		t.Errorf("api key = %q, want obx_v3", secrets[EnvAPIKeyDirect])
	}
	if secrets[EnvWorkloadPrivateKey] != "dGhlLXYzLWtleQ==" {
		t.Errorf("workload private key = %q, want the new value", secrets[EnvWorkloadPrivateKey])
	}
	if _, ok := secrets[EnvAgentPrivateKey]; ok {
		t.Error("legacy OPENBOX_AGENT_PRIVATE_KEY survived a v3 identity write")
	}

	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf("token cache still exists after a fresh identity write (err=%v)", err)
	}

	ls, err := LegacyStoreFor("codex")
	if err != nil {
		t.Fatal(err)
	}
	if ls.Legacy {
		t.Errorf("LegacyStoreFor reports legacy after a v3 write: %v", ls.Reasons)
	}

	// Second invocation: byte-identical files.
	cfgBefore, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	envBefore, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := WriteWorkloadIdentity("codex", id); err != nil {
		t.Fatalf("second WriteWorkloadIdentity: %v", err)
	}

	cfgAfter, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	envAfter, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(cfgBefore) != string(cfgAfter) {
		t.Errorf("dev.json changed on a second identical write:\nbefore=%s\nafter=%s", cfgBefore, cfgAfter)
	}
	if string(envBefore) != string(envAfter) {
		t.Errorf(".env changed on a second identical write:\nbefore=%s\nafter=%s", envBefore, envAfter)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf("token cache reappeared after a second write (err=%v)", err)
	}
}

// TestWriteWorkloadIdentityToleratesAnAbsentTokenCache the delete step must
// not fail a first-ever registration, which never had a cache to begin with.
func TestWriteWorkloadIdentityToleratesAnAbsentTokenCache(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	t.Setenv(EnvConfigPath, "")

	err := WriteWorkloadIdentity("codex", WorkloadIdentity{
		AgentID: "agent-1", APIKey: "obx_a", PrivateKeyB64: "a2V5",
	})
	if err != nil {
		t.Fatalf("WriteWorkloadIdentity with no prior cache: %v", err)
	}
}
