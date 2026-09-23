package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client/workloadauth"
)

// TestDoctorNamesLegacyStoreHooksSendNothing: doctor is the only in-product
// place that says a legacy store's hooks send nothing, and it has to say so
// however the store shows its age -- a stored developer_did, an Ed25519
// signing seed with no DID at all, or both at once.
func TestDoctorNamesLegacyStoreHooksSendNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, home string)
	}{
		{"stored DID", func(t *testing.T, home string) {
			if err := devconfig.SetLegacyDID(filepath.Join(home, "claude-code", "dev.json"), "did:aip:legacy-store"); err != nil {
				t.Fatal(err)
			}
		}},
		{"seed only", func(t *testing.T, home string) {
			envPath, err := devconfig.EnvFilePathFor("claude-code")
			if err != nil {
				t.Fatal(err)
			}
			if err := devconfig.WriteEnvFile(envPath, map[string]string{
				devconfig.EnvAgentPrivateKey: "legacy-ed25519-seed-value"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"both", func(t *testing.T, home string) {
			if err := devconfig.SetLegacyDID(filepath.Join(home, "claude-code", "dev.json"), "did:aip:legacy-store"); err != nil {
				t.Fatal(err)
			}
			envPath, err := devconfig.EnvFilePathFor("claude-code")
			if err != nil {
				t.Fatal(err)
			}
			if err := devconfig.WriteEnvFile(envPath, map[string]string{
				devconfig.EnvAgentPrivateKey: "legacy-ed25519-seed-value"}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := isolateHomeUnbound(t)
			requireUnbound(t)
			t.Setenv(envManagedSettingsPath, filepath.Join(t.TempDir(), "absent.json"))
			tc.seed(t, home)

			out, code := runDoctorHere(t)
			if code != exitOK {
				t.Fatalf("doctor exit = %d:\n%s", code, out)
			}
			if got := strings.Count(out, "hooks send nothing"); got != 1 {
				t.Errorf("doctor names claude-code's legacy store %d times, want 1:\n%s", got, out)
			}
			if !strings.Contains(out, "LEGACY (pre-IAMv3) identity") {
				t.Errorf("doctor does not label the row LEGACY:\n%s", out)
			}
			if !strings.Contains(out, "openbox init --provider claude-code") {
				t.Errorf("doctor does not name the remedy:\n%s", out)
			}
		})
	}
}

// TestDoctorV3RowLabelsAttributionAsDerived: a workload agent has no DID at
// the backend, so its row must frame the label as "attribution ... (derived)"
// and no row may read "DID did:aip..." as though the agent owned one.
func TestDoctorV3RowLabelsAttributionAsDerived(t *testing.T) {
	isolateHomeUnbound(t)
	requireUnbound(t)
	t.Setenv(envManagedSettingsPath, filepath.Join(t.TempDir(), "absent.json"))
	seedToolCredentials(t, "claude-code", testAgentIDFor(t, "claude-code"))

	out, code := runDoctorHere(t)
	if code != exitOK {
		t.Fatalf("doctor exit = %d:\n%s", code, out)
	}
	if !strings.Contains(out, "attribution "+testDIDFor(t, "claude-code")+" (derived)") {
		t.Errorf("doctor does not label the attribution as derived:\n%s", out)
	}
	if disallowed := regexp.MustCompile(`(?m)^\s*\S+\s+.*\bDID did:aip`); disallowed.MatchString(out) {
		t.Errorf("a row reads DID did:aip as though the agent owned one:\n%s", out)
	}
}

// TestDoctorKidMatchesRegisteredThumbprint the kid doctor reports has to be
// the same value the backend holds -- the RFC 7638 thumbprint of the
// registered key's public half -- and never the key itself.
func TestDoctorKidMatchesRegisteredThumbprint(t *testing.T) {
	isolateHomeUnbound(t)
	requireUnbound(t)
	t.Setenv(envManagedSettingsPath, filepath.Join(t.TempDir(), "absent.json"))
	seedToolCredentials(t, "claude-code", testAgentIDFor(t, "claude-code"))

	key, err := workloadauth.ParsePrivateKey(testWorkloadKeyOnce)
	if err != nil {
		t.Fatal(err)
	}
	want, err := workloadauth.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	out, code := runDoctorHere(t)
	if code != exitOK {
		t.Fatalf("doctor exit = %d:\n%s", code, out)
	}
	if !strings.Contains(out, "key kid "+want) {
		t.Errorf("doctor does not report the registered key's thumbprint %q:\n%s", want, out)
	}
	if strings.Contains(out, testWorkloadKeyOnce) {
		t.Errorf("the private key reached stdout:\n%s", out)
	}
}

// cacheFixtureToken is built from fragments rather than a single literal, so
// it never reads as a secret-shaped assignment on disk; its only job here is
// to prove it never reaches stdout.
var cacheFixtureToken = "fixture" + "-cached-bearer-" + "token"

// writeTokenCacheFixture writes only the fields doctor's cache reader is
// documented to touch (expires_at), plus a token value the test can search
// for in stdout -- proving the read path never surfaces it.
func writeTokenCacheFixture(t *testing.T, path string, expiresAt time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	entry := struct {
		AccessToken string    `json:"access_token"`
		ExpiresAt   time.Time `json:"expires_at"`
	}{AccessToken: cacheFixtureToken, ExpiresAt: expiresAt}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDoctorCacheStates covers every cache-line state the security review
// pinned: absent, a live token, an expired one, and a file this process
// cannot parse. None of them may ever print the cached token.
func TestDoctorCacheStates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, path string)
		want  string
	}{
		{"absent", func(t *testing.T, path string) {}, "token cache: absent"},
		{"valid", func(t *testing.T, path string) {
			writeTokenCacheFixture(t, path, time.Now().Add(4*time.Minute))
		}, "token cache: valid for"},
		{"expired", func(t *testing.T, path string) {
			writeTokenCacheFixture(t, path, time.Now().Add(-4*time.Minute))
		}, "token cache: expired"},
		{"corrupt", func(t *testing.T, path string) {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "token cache: unreadable (ignored; the next call re-authenticates)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHomeUnbound(t)
			requireUnbound(t)
			t.Setenv(envManagedSettingsPath, filepath.Join(t.TempDir(), "absent.json"))
			seedToolCredentials(t, "claude-code", testAgentIDFor(t, "claude-code"))

			cachePath, err := devconfig.WorkloadTokenCachePathFor("claude-code")
			if err != nil {
				t.Fatal(err)
			}
			tc.write(t, cachePath)

			out, code := runDoctorHere(t)
			if code != exitOK {
				t.Fatalf("doctor exit = %d:\n%s", code, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("doctor does not report %q:\n%s", tc.want, out)
			}
			if strings.Contains(out, cacheFixtureToken) {
				t.Errorf("the cached token reached stdout:\n%s", out)
			}
		})
	}
}

// TestDoctorFlagsRetiredExportsAsIgnored the v1 signing-identity exports are
// read by nothing at runtime; a developer or an org who set one on purpose has
// to be told it does nothing, distinctly from the v3 exports that legitimately
// shadow every file.
func TestDoctorFlagsRetiredExportsAsIgnored(t *testing.T) {
	isolateHomeUnbound(t)
	requireUnbound(t)
	t.Setenv(envManagedSettingsPath, filepath.Join(t.TempDir(), "absent.json"))
	t.Setenv(devconfig.EnvDID, "did:aip:should-be-ignored")
	t.Setenv(devconfig.EnvAgentPrivateKey, "legacy-signing-key-value")

	out, code := runDoctorHere(t)
	if code != exitOK {
		t.Fatalf("doctor exit = %d:\n%s", code, out)
	}
	for _, name := range []string{devconfig.EnvDID, devconfig.EnvAgentPrivateKey} {
		if !strings.Contains(out, name) {
			t.Errorf("doctor does not name the retired export %s:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "set but IGNORED (retired with the v1 signing path)") {
		t.Errorf("doctor does not flag the retired exports as ignored:\n%s", out)
	}
	if strings.Contains(out, "this outranks every file above") {
		t.Errorf("a retired export was treated as one that shadows every file:\n%s", out)
	}
}
