package devconfig

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLegacyStoreDetection tables every combination LegacyStoreFor must
// distinguish: a stored DID, a seed, both, a clean v3 store, an empty
// directory, and a seed that exists only in the environment (not legacy: the
// function reads files only).
func TestLegacyStoreDetection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		did        string
		envSeedKey string // written to the .env FILE when non-empty
		exportSeed bool   // set only as a real environment variable
		wantLegacy bool
	}{
		{name: "stored DID only", did: "did:aip:legacy", wantLegacy: true},
		{name: "seed only", envSeedKey: EnvAgentPrivateKey, wantLegacy: true},
		{name: "deprecated seed alias only", envSeedKey: "OPENBOX_ED25519_SEED", wantLegacy: true},
		{name: "both DID and seed", did: "did:aip:legacy", envSeedKey: EnvAgentPrivateKey, wantLegacy: true},
		{name: "clean v3 store", wantLegacy: false},
		{name: "empty dir (not legacy)", wantLegacy: false},
		{name: "env-only seed is not legacy: files only", exportSeed: true, wantLegacy: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv(EnvHome, home)
			t.Setenv(EnvConfigPath, "")

			if tc.did != "" || tc.name == "clean v3 store" {
				u := Update{}
				if tc.did != "" {
					u.DID = tc.did
				} else {
					u.AgentID = "agent-1"
					u.IdentityMethod = IdentityMethodKeycloakWorkload
				}
				cfgPath, err := DevConfigWritePathFor("codex")
				if err != nil {
					t.Fatal(err)
				}
				if err := WriteConfig(cfgPath, u); err != nil {
					t.Fatal(err)
				}
			}

			if tc.envSeedKey != "" {
				envPath, err := EnvFilePathFor("codex")
				if err != nil {
					t.Fatal(err)
				}
				if err := WriteEnvFile(envPath, map[string]string{tc.envSeedKey: "c2VlZA=="}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.exportSeed {
				t.Setenv(EnvAgentPrivateKey, "c2VlZA==")
			}

			ls, err := LegacyStoreFor("codex")
			if err != nil {
				t.Fatalf("LegacyStoreFor: %v", err)
			}
			if ls.Legacy != tc.wantLegacy {
				t.Errorf("Legacy = %t, want %t (reasons=%v)", ls.Legacy, tc.wantLegacy, ls.Reasons)
			}
			if tc.wantLegacy && len(ls.Reasons) == 0 {
				t.Error("Legacy=true but Reasons is empty; doctor/the hook need something to print")
			}
			if !tc.wantLegacy && len(ls.Reasons) != 0 {
				t.Errorf("Legacy=false but Reasons = %v, want none", ls.Reasons)
			}
		})
	}
}

// TestTokenCacheIsNotACredentialSource a valid-looking cache file with no
// .env must resolve no API key and no workload key: the cache is derived, not
// a store, and resolveCredentialsFrom must never read it as one.
func TestTokenCacheIsNotACredentialSource(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvDID, testDID)

	cachePath, err := WorkloadTokenCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"access_token":"looks-like-a-bearer","expires_at":9999999999,"client_id":"c","kid":"k"}`
	if err := os.WriteFile(cachePath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := ResolveCredentials()
	if err == nil {
		t.Fatalf("ResolveCredentials() succeeded from a token cache alone: %+v", c)
	}
	if c.APIKey != "" || c.WorkloadPrivateKey != "" {
		t.Errorf("credentials leaked from the token cache: %+v", c)
	}
}
