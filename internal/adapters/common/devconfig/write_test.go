package devconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// TestWriteConfig_ReInitKeepsPostureItWasNotGiven a plain `init` (to repair
// hooks, refresh the bundle, anything) must not drop an explicit posture
// opt-in with exit 0 and no message, because the installers rebuild dev.json
// from the current run's flags and only carry forward the sync coordinates.
// Enforce itself has no Update field any more (no install-time knob left at
// all, since ResolveEnforce always reports true) -- this now proves the
// general merge behavior against Tier2/Findings instead.
func TestWriteConfig_ReInitKeepsPostureItWasNotGiven(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.json")

	if err := WriteConfig(path, Update{
		Tier2:    boolPtr(true),
		Findings: boolPtr(true),
	}); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	if err := WriteConfig(path, Update{BaseURL: "https://core.example"}); err != nil {
		t.Fatalf("re-init write: %v", err)
	}

	cfg := mustLoad(t, path)
	if cfg.Tier2 == nil || !*cfg.Tier2 {
		t.Error("re-init dropped tier2")
	}
	if cfg.Findings == nil || !*cfg.Findings {
		t.Error("re-init dropped findings")
	}
	if cfg.BaseURL != "https://core.example" {
		t.Errorf("re-init did not apply the new value: base_url = %q", cfg.BaseURL)
	}
}

// TestWriteConfig_KeepsCoordinatesItWasNotGiven the reuse path resolves the
// DID from the secret store but not the coordinates, so a re-init used to
// blank fields it simply had nothing to say about.
func TestWriteConfig_KeepsCoordinatesItWasNotGiven(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.json")
	if err := WriteConfig(path, Update{
		BaseURL:    "https://core.example",
		AgentID:    "agent-1",
		BackendURL: "https://backend.example",
	}); err != nil {
		t.Fatal(err)
	}
	if err := setLegacyDID(path, "did:aip:original"); err != nil {
		t.Fatal(err)
	}

	if err := WriteConfig(path, Update{BaseURL: "https://core.example"}); err != nil {
		t.Fatal(err)
	}

	cfg := mustLoad(t, path)
	for _, c := range []struct{ name, got, want string }{
		{"developer_did", cfg.DID, "did:aip:original"},
		{"base_url", cfg.BaseURL, "https://core.example"},
		{"agent_id", cfg.AgentID, "agent-1"},
		{"backend_url", cfg.BackendURL, "https://backend.example"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q (a re-init must not erase what it was not given)", c.name, c.got, c.want)
		}
	}
}

// TestWriteConfig_KeepsFieldsTheUpdateCannotExpress the merge starts from what
// is on disk, so a field this writer has never heard of survives.
func TestWriteConfig_KeepsFieldsTheUpdateCannotExpress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.json")
	seed := DevConfig{
		DID:              "did:aip:x",
		Finops:           boolPtr(true),
		FailClosed:       boolPtr(true),
		Tier2TimeoutMS:   2500,
		OrgSigningPubKey: "Zm9vYmFy",
		SecretDetection:  boolPtr(false),
	}
	raw, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteConfig(path, Update{ContentCapture: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}

	cfg := mustLoad(t, path)
	if cfg.Finops == nil || !*cfg.Finops || cfg.FailClosed == nil || !*cfg.FailClosed || cfg.Tier2TimeoutMS != 2500 ||
		cfg.OrgSigningPubKey != "Zm9vYmFy" ||
		cfg.SecretDetection == nil || *cfg.SecretDetection {
		t.Errorf("hand-tuned settings did not survive a re-init: %+v", cfg)
	}
}

// `init` is also the repair command, so it has to work against a config it
// cannot parse rather than refusing and leaving the developer stuck.
func TestWriteConfig_OverwritesUnparseablePriorConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfig(path, Update{ContentCapture: boolPtr(true)}); err != nil {
		t.Fatalf("write over a corrupt config: %v", err)
	}
	cfg := mustLoad(t, path)
	if cfg.ContentCapture == nil || !*cfg.ContentCapture {
		t.Errorf("recovery write did not take: %+v", cfg)
	}
}

func TestWriteConfig_FilePermissionsAreOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	path := filepath.Join(dir, "dev.json")
	if err := WriteConfig(path, Update{}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("dev.json mode = %o, want 600", got)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("config dir mode = %o, want 700", got)
	}
}

// TestWriteConfigClearsLegacyDIDOnWorkloadIdentity a re-init into a legacy
// store must not leave the old DID sitting beside the new identity_method:
// setString cannot clear a field, so this needs the explicit clear.
func TestWriteConfigClearsLegacyDIDOnWorkloadIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.json")
	if err := setLegacyDID(path, "did:aip:legacy"); err != nil {
		t.Fatal(err)
	}
	if got := mustLoad(t, path).DID; got != "did:aip:legacy" {
		t.Fatalf("seed did not take: DID = %q", got)
	}

	if err := WriteConfig(path, Update{
		AgentID:        "agent-1",
		IdentityMethod: IdentityMethodKeycloakWorkload,
	}); err != nil {
		t.Fatal(err)
	}
	cfg := mustLoad(t, path)
	if cfg.DID != "" {
		t.Errorf("DID = %q, want it cleared by a keycloak_workload write", cfg.DID)
	}
	if cfg.IdentityMethod != IdentityMethodKeycloakWorkload {
		t.Errorf("identity_method = %q, want %q", cfg.IdentityMethod, IdentityMethodKeycloakWorkload)
	}
	if cfg.AgentID != "agent-1" {
		t.Errorf("agent_id = %q, want agent-1", cfg.AgentID)
	}
}

// TestWriteConfigKeepsDIDWithoutIdentityMethod no behaviour change for
// today's callers: an Update that never mentions IdentityMethod must not
// touch the DID, which is exactly what every existing caller does.
func TestWriteConfigKeepsDIDWithoutIdentityMethod(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.json")
	if err := setLegacyDID(path, "did:aip:x"); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfig(path, Update{BaseURL: "https://core.example"}); err != nil {
		t.Fatal(err)
	}
	cfg := mustLoad(t, path)
	if cfg.DID != "did:aip:x" {
		t.Errorf("DID = %q, want it left alone by a write that never mentions identity_method", cfg.DID)
	}
	if cfg.IdentityMethod != "" {
		t.Errorf("identity_method = %q, want empty", cfg.IdentityMethod)
	}
}

func mustLoad(t *testing.T, path string) DevConfig {
	t.Helper()
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return cfg
}
