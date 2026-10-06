package devinit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"

	claudecode "github.com/openbox-ai/openbox-shift-left/internal/adapters/claude-code"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/backend"
)

func mockCreateServer(t *testing.T, createBody *map[string]any) *memhttptest.Server {
	t.Helper()
	return memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/agent/list":
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/agent/create":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, createBody)
			// A real backend persists exactly the kid this machine submitted; echoed
			// here so the guard on the client side (registered kid must match the
			// submitted JWK) does not reject its own fixture.
			kid := "kid-1"
			if iv, ok := (*createBody)["identity_verification"].(map[string]any); ok {
				if pj, ok := iv["public_jwk"].(map[string]any); ok {
					if k, _ := pj["kid"].(string); k != "" {
						kid = k
					}
				}
			}
			fmt.Fprintf(w, `{"data":{"agent":{"id":"srv-agent","agent_name":"dev-x","tier":"Tier 2","trust_score":0.81},`+
				`"token":"obx_test_%s",`+
				`"identity":{"method":"keycloak_workload","source_type":"openbox","workload_identity_id":"wi-1",`+
				`"credential_id":"cred-1","service_account_id":"sa-1","client_id":"client-1","kid":%q,`+
				`"token_endpoint":"https://idp.example/token","audience":"aud","private_key_available_from_openbox":false}}}`,
				strings.Repeat("a", 48), kid)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
}

// TestEndToEndClaudeCodeRealInstall is the end-to-end acceptance: an init for
// claude-code against a mock backend + a temp-HOME install materializes the
// plugin bundle and the non-secret dev config, and NO written file contains a
// secret value (INV-1).
func TestEndToEndClaudeCodeRealInstall(t *testing.T) {
	var createBody map[string]any
	srv := mockCreateServer(t, &createBody)
	defer srv.Close()

	pluginDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "openbox", "dev.json")
	inst := claudecode.Installer{
		PluginDir:  pluginDir,
		ConfigPath: cfgPath,
		// Pinned, or the install registers hooks in the real ~/.claude.
		SettingsPath: filepath.Join(t.TempDir(), ".claude", "settings.json"),
	}

	home := t.TempDir()
	t.Setenv(devconfig.EnvHome, home)
	bindProviderForTest(t, "claude-code")
	reg := backend.New(srv.URL, "obx_key_"+strings.Repeat("f", 48), "openbox-cli")
	var out bytes.Buffer

	res, err := Run(context.Background(),
		Options{Provider: "claude-code", AgentName: "dev-x"},
		Deps{GenerateKey: fixedGenerateKey, Registrar: reg, Installer: inst, Out: &out})
	if err != nil {
		t.Fatalf("expected a clean install, got err=%v", err)
	}
	if !res.Registered || !res.ConfigApplied {
		t.Fatalf("expected registered+config-applied, got %+v", res)
	}

	// The bundle hosts the engine and nothing else: a plugin manifest would make
	// the directory loadable, and a plugin's handlers do not de-duplicate
	// against the settings-level registrations the install writes.
	if _, err := os.Stat(filepath.Join(pluginDir, "bin")); err != nil {
		t.Errorf("the bundle has no bin/ for the engine: %v", err)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read dev config: %v", err)
	}
	var cfg claudecode.DevConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse dev config: %v", err)
	}
	if cfg.DID != "" {
		t.Errorf("dev config DID = %q, want empty: a v3 store never persists one", cfg.DID)
	}
	if cfg.AgentID != "srv-agent" {
		t.Errorf("dev config agent_id = %q, want srv-agent", cfg.AgentID)
	}
	if cfg.IdentityMethod != devconfig.IdentityMethodKeycloakWorkload {
		t.Errorf("dev config identity_method = %q, want %s", cfg.IdentityMethod, devconfig.IdentityMethodKeycloakWorkload)
	}

	assertNoSecretInTree(t, pluginDir)
	if strings.Contains(string(raw), "obx_") {
		t.Errorf("dev config leaked a secret value:\n%s", raw)
	}
	kv := readCredentialFile(t)
	if v := kv[devconfig.EnvAPIKeyDirect]; !strings.HasPrefix(v, "obx_test_") {
		t.Errorf("api key not written: %q", v)
	}
	if v := kv[devconfig.EnvWorkloadPrivateKey]; v == "" {
		t.Errorf("workload private key not written")
	}
}

func assertNoSecretInTree(t *testing.T, dir string) {
	t.Helper()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "obx_test_") || strings.Contains(string(b), "c2VlZA==") {
			t.Errorf("secret value leaked into bundle file %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}
