package devinit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/backend"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/provider"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/secret"
)

// TestEndToEndAgainstMockBackend drives the real backend.Client (not a fake)
// through devinit against an httptest server, covering the SL-2 integration AC
// ("a dev init against a test OpenBox registers an agent") with a mock.
func TestEndToEndAgainstMockBackend(t *testing.T) {
	var createBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/agent/list":
			_, _ = io.WriteString(w, `{"data":[]}`) // no existing agent
		case r.Method == http.MethodPost && r.URL.Path == "/agent/create":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &createBody)
			_, _ = io.WriteString(w, `{"data":{"agent":{"id":"srv-agent","agent_name":"dev-x","did":"did:aip:server","tier":"Tier 2","trust_score":0.81},"token":"obx_test_`+strings.Repeat("a", 48)+`","identity":{"did":"did:aip:server","privateKey":"c2VlZA=="}}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	reg := backend.New(srv.URL, "obx_key_"+strings.Repeat("f", 48), "openbox-cli")
	store := secret.NewMemStore()
	inst, _ := provider.Lookup("claude-code") // real stub: adapter not built
	var out bytes.Buffer

	res, err := Run(context.Background(),
		Options{Provider: "claude-code", Org: "acme", AgentName: "dev-x"},
		Deps{Registrar: reg, Store: store, Installer: inst, Out: &out})

	// Claude Code adapter isn't built, so config is manual-only (expected error),
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
	svc, apiAcct, privAcct, didAcct := Options{Provider: "claude-code", Org: "acme"}.accounts()
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
