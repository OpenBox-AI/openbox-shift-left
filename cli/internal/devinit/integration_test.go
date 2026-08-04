package devinit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	claudecode "github.com/openbox-ai/openbox-shift-left/adapters/claude-code"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/backend"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/providers"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/secret"
)

// mockCreateServer is an httptest backend that reports no existing agent and
// returns a fixed registration on agent/create, capturing the request body.
func mockCreateServer(t *testing.T, createBody *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/agent/list":
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/agent/create":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, createBody)
			_, _ = io.WriteString(w, `{"data":{"agent":{"id":"srv-agent","agent_name":"dev-x","did":"did:aip:server","tier":"Tier 2","trust_score":0.81},"token":"obx_test_`+strings.Repeat("a", 48)+`","identity":{"did":"did:aip:server","privateKey":"c2VlZA=="}}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
}

// TestEndToEndAgainstMockBackend drives the real backend.Client (not a fake)
// through devinit against an httptest server, covering the SL-2 integration AC
// ("a init against a test OpenBox registers an agent") with a mock. It uses
// cursor — still a Stub (SL-8 unbuilt) — so the config-manual-only partial path
// stays exercised now that claude-code (SL4-WIRE-1) AND codex (STORY-SL7-A) are
// real installers. (It previously used codex; a real installer here would write
// the developer's ACTUAL default install paths from a unit test.)
func TestEndToEndAgainstMockBackend(t *testing.T) {
	var createBody map[string]any
	srv := mockCreateServer(t, &createBody)
	defer srv.Close()

	reg := backend.New(srv.URL, "obx_key_"+strings.Repeat("f", 48), "openbox-cli")
	store := secret.NewMemStore()
	inst, _ := providers.Lookup("cursor") // stub: SL-8 adapter not built
	var out bytes.Buffer

	res, err := Run(context.Background(),
		Options{Provider: "cursor", Org: "acme", AgentName: "dev-x"},
		Deps{Registrar: reg, Store: store, Installer: inst, Out: &out})

	// The cursor adapter isn't built, so config is manual-only (expected error),
	// but registration + credential capture must have fully succeeded.
	if err == nil || !res.ConfigManualOnly {
		t.Fatalf("expected manual-config outcome, got err=%v res=%+v", err, res)
	}
	if !res.Registered || res.AgentID != "srv-agent" || res.DID != "did:aip:server" {
		t.Fatalf("registration not captured: %+v", res)
	}
	// The DTO the CLI sent must satisfy the backend's required fields.
	if createBody["agent_type"] != "developer" {
		t.Errorf("agent_type = %v", createBody["agent_type"])
	}
	if s, _ := createBody["icon"].(string); s == "" {
		t.Error("icon must be non-empty")
	}
	if _, ok := createBody["aivss_config"].(map[string]any); !ok {
		t.Error("aivss_config must be an object")
	}
	// Credentials landed in the store, keyed by org/provider.
	svc, apiAcct, privAcct, didAcct := Options{Provider: "cursor", Org: "acme"}.accounts()
	if v, _ := store.Get(svc, apiAcct); !strings.HasPrefix(v, "obx_test_") {
		t.Errorf("api key not stored: %q", v)
	}
	if v, _ := store.Get(svc, privAcct); v != "c2VlZA==" {
		t.Errorf("private key not stored: %q", v)
	}
	if v, _ := store.Get(svc, didAcct); v != "did:aip:server" {
		t.Errorf("did not stored: %q", v)
	}
}

// TestEndToEndClaudeCodeRealInstall is the SL4-WIRE-1 acceptance: a init for
// claude-code against a mock backend + a temp-HOME install materializes the
// plugin bundle and the non-secret dev config, and NO written file contains a
// secret value (INV-1). It uses the real claudecode.Installer (what the CLI
// registers) pointed at temp dirs.
func TestEndToEndClaudeCodeRealInstall(t *testing.T) {
	var createBody map[string]any
	srv := mockCreateServer(t, &createBody)
	defer srv.Close()

	pluginDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "openbox", "dev.json")
	inst := claudecode.Installer{PluginDir: pluginDir, ConfigPath: cfgPath}

	reg := backend.New(srv.URL, "obx_key_"+strings.Repeat("f", 48), "openbox-cli")
	store := secret.NewMemStore()
	var out bytes.Buffer

	res, err := Run(context.Background(),
		Options{Provider: "claude-code", Org: "acme", AgentName: "dev-x"},
		Deps{Registrar: reg, Store: store, Installer: inst, Out: &out})
	if err != nil {
		t.Fatalf("expected a clean install, got err=%v", err)
	}
	if !res.Registered || !res.ConfigApplied || res.ConfigManualOnly {
		t.Fatalf("expected registered+config-applied, got %+v", res)
	}

	// Bundle materialized (including the dotted .claude-plugin dir).
	for _, rel := range []string{".claude-plugin/plugin.json", "hooks/hooks.json"} {
		if _, err := os.Stat(filepath.Join(pluginDir, rel)); err != nil {
			t.Errorf("missing bundle file %s: %v", rel, err)
		}
	}

	// Dev config written with the secret-store coordinates, not the values.
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read dev config: %v", err)
	}
	var cfg claudecode.DevConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse dev config: %v", err)
	}
	if cfg.DID != "did:aip:server" {
		t.Errorf("dev config DID = %q", cfg.DID)
	}

	// INV-1: no file written under the plugin dir or the config path may contain
	// a secret value. The obx_ key and the base64 seed both landed in the store.
	assertNoSecretInTree(t, pluginDir)
	if strings.Contains(string(raw), "obx_") || strings.Contains(string(raw), "c2VlZA==") {
		t.Errorf("dev config leaked a secret value:\n%s", raw)
	}
	svc, apiAcct, privAcct, _ := Options{Provider: "claude-code", Org: "acme"}.accounts()
	if v, _ := store.Get(svc, apiAcct); !strings.HasPrefix(v, "obx_test_") {
		t.Errorf("api key not stored: %q", v)
	}
	if v, _ := store.Get(svc, privAcct); v != "c2VlZA==" {
		t.Errorf("private key not stored: %q", v)
	}
}

// assertNoSecretInTree walks dir and fails if any file contains an obx_ key or
// the test seed value — the INV-1 leak check for the materialized bundle.
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
