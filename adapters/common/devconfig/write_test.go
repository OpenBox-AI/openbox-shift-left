package devconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// The bug this file exists for: `init --enforce` followed by a plain
// `init` (to repair hooks, refresh the bundle, anything) used to drop the
// developer from enforce to observe with exit 0 and no message, because the
// installers rebuilt dev.json from the current run's flags and only carried
// forward the sync coordinates.
func TestWriteConfig_ReInitKeepsEnforcePosture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.json")

	if err := WriteConfig(path, Update{
		DID:      "did:aip:x",
		Enforce:  boolPtr(true),
		Tier2:    boolPtr(true),
		Findings: boolPtr(true),
	}); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	// A re-init that says nothing about posture.
	if err := WriteConfig(path, Update{DID: "did:aip:x", SecretService: "svc"}); err != nil {
		t.Fatalf("re-init write: %v", err)
	}

	cfg := mustLoad(t, path)
	if !cfg.Enforce {
		t.Error("re-init turned enforcement off; a run that never mentioned it must leave it alone")
	}
	if cfg.Tier2 == nil || !*cfg.Tier2 {
		t.Error("re-init dropped tier2")
	}
	if cfg.Findings == nil || !*cfg.Findings {
		t.Error("re-init dropped findings")
	}
	if cfg.SecretService != "svc" {
		t.Errorf("re-init did not apply the new value: secret_service = %q", cfg.SecretService)
	}
}

// Preserving on silence would be a one-way ratchet without an explicit way back
// down, so --no-enforce has to actually work.
func TestWriteConfig_ExplicitDowngradeApplies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.json")
	if err := WriteConfig(path, Update{DID: "did:aip:x", Enforce: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}

	u := Update{DID: "did:aip:x", Enforce: boolPtr(false)}
	if !WouldDowngradeEnforce(path, u.Enforce) {
		t.Error("an explicit false against a prior true must be reported as a downgrade so the CLI can say so")
	}
	if err := WriteConfig(path, u); err != nil {
		t.Fatal(err)
	}
	if mustLoad(t, path).Enforce {
		t.Error("--no-enforce did not turn enforcement off")
	}
}

// Silence is not a downgrade — otherwise every ordinary re-init would print a
// posture warning.
func TestWriteConfig_SilenceIsNotADowngrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.json")
	if err := WriteConfig(path, Update{DID: "did:aip:x", Enforce: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	if WouldDowngradeEnforce(path, nil) {
		t.Error("a run that does not mention enforce must not report a downgrade")
	}
}

// The reuse path resolves the DID from the secret store but not the coordinates,
// so a re-init used to blank fields it simply had nothing to say about.
func TestWriteConfig_KeepsCoordinatesItWasNotGiven(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.json")
	if err := WriteConfig(path, Update{
		DID:        "did:aip:original",
		BaseURL:    "https://core.example",
		AgentID:    "agent-1",
		BackendURL: "https://backend.example",
	}); err != nil {
		t.Fatal(err)
	}

	if err := WriteConfig(path, Update{SecretService: "svc"}); err != nil {
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

// The merge starts from what is on disk, so a field this writer has never heard
// of survives. That is what keeps the policy from rotting as DevConfig grows —
// the previous implementations listed the fields to preserve, and the list was
// already incomplete.
func TestWriteConfig_KeepsFieldsTheUpdateCannotExpress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.json")
	seed := DevConfig{
		DID:              "did:aip:x",
		Finops:           true,
		FailClosed:       true,
		Tier2TimeoutMS:   2500,
		SecretFile:       "secrets-e2e.json",
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

	if err := WriteConfig(path, Update{Enforce: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}

	cfg := mustLoad(t, path)
	if !cfg.Finops || !cfg.FailClosed || cfg.Tier2TimeoutMS != 2500 ||
		cfg.SecretFile != "secrets-e2e.json" || cfg.OrgSigningPubKey != "Zm9vYmFy" ||
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
	if err := WriteConfig(path, Update{DID: "did:aip:x", Enforce: boolPtr(true)}); err != nil {
		t.Fatalf("write over a corrupt config: %v", err)
	}
	cfg := mustLoad(t, path)
	if cfg.DID != "did:aip:x" || !cfg.Enforce {
		t.Errorf("recovery write did not take: %+v", cfg)
	}
}

func TestWriteConfig_FilePermissionsAreOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	path := filepath.Join(dir, "dev.json")
	if err := WriteConfig(path, Update{DID: "did:aip:x"}); err != nil {
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

func mustLoad(t *testing.T, path string) DevConfig {
	t.Helper()
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return cfg
}
