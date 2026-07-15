package backend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/aivss"
)

func TestCreateParsesResponseAndSendsContract(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotClient, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/agent/create" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("X-API-Key")
		gotClient = r.Header.Get("x-openbox-client")
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"agent":{"id":"a-1","agent_name":"dev","did":"did:aip:x","tier":"Tier 2","trust_score":0.81},"token":"obx_test_`+repeat("a", 48)+`","identity":{"did":"did:aip:x","privateKey":"c2VlZA=="}}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "obx_key_"+repeat("f", 48), "openbox-cli")
	reg, err := c.Create(context.Background(), CreateAgentRequest{
		AgentName:   "dev",
		AgentType:   "developer",
		Icon:        "🧑‍💻",
		AivssConfig: aivss.DefaultDeveloperProfile(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// obx_key_ credential must go on X-API-Key, and the JWT-only x-openbox-client
	// header must NOT be sent on the API-key path.
	if gotAuth == "" {
		t.Error("expected X-API-Key header to be set")
	}
	if gotClient != "" {
		t.Errorf("x-openbox-client should be empty on the API-key path, got %q", gotClient)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	// Contract fields the backend requires.
	if gotBody["agent_type"] != "developer" {
		t.Errorf("agent_type = %v, want developer", gotBody["agent_type"])
	}
	if gotBody["icon"] == "" || gotBody["icon"] == nil {
		t.Error("icon must be non-empty (backend DTO @IsNotEmpty)")
	}
	if _, ok := gotBody["aivss_config"]; !ok {
		t.Error("aivss_config must be present")
	}
	// Parsed credentials.
	if reg.APIKey == "" || reg.PrivateKey != "c2VlZA==" || reg.DID != "did:aip:x" {
		t.Errorf("bad registration parse: %+v", reg)
	}
	if reg.AgentID != "a-1" || reg.Tier != "Tier 2" {
		t.Errorf("bad agent fields: %+v", reg)
	}
}

func TestCreateBearerPathSetsClientHeader(t *testing.T) {
	var gotAuth, gotClient string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotClient = r.Header.Get("x-openbox-client")
		_, _ = io.WriteString(w, `{"data":{"agent":{"id":"a"},"token":"t","identity":{"did":"d","privateKey":"p"}}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "eyJ.jwt.token", "my-client")
	if _, err := c.Create(context.Background(), CreateAgentRequest{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if gotAuth != "Bearer eyJ.jwt.token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotClient != "my-client" {
		t.Errorf("x-openbox-client = %q, want my-client", gotClient)
	}
}

func TestCreateDIDFallsBackToAgentBody(t *testing.T) {
	// F6: identity is not in the advertised Swagger DTO; when identity.did is
	// absent the client must fall back to agent.did.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"agent":{"id":"a","did":"did:aip:from-agent"},"token":"t","identity":{"privateKey":"p"}}}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "obx_key_x", "cli")
	reg, err := c.Create(context.Background(), CreateAgentRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if reg.DID != "did:aip:from-agent" {
		t.Errorf("DID = %q, want fallback to agent.did", reg.DID)
	}
}

func TestCreateAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"AIVSS config is required"}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "obx_key_x", "cli")
	_, err := c.Create(context.Background(), CreateAgentRequest{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %v", err)
	}
	if apiErr.StatusCode != 400 {
		t.Errorf("status = %d, want 400", apiErr.StatusCode)
	}
}

func TestFindByName(t *testing.T) {
	cases := map[string]string{
		"bare array": `{"data":[{"id":"1","agent_name":"alpha"},{"id":"2","agent_name":"Beta"}]}`,
		"paged":      `{"data":{"items":[{"id":"1","agent_name":"alpha"},{"id":"2","agent_name":"Beta"}]}}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/agent/list" {
					t.Errorf("path = %s", r.URL.Path)
				}
				_, _ = io.WriteString(w, payload)
			}))
			defer srv.Close()
			c := New(srv.URL, "obx_key_x", "cli")

			// Case-insensitive match.
			got, err := c.FindByName(context.Background(), "beta")
			if err != nil {
				t.Fatalf("FindByName: %v", err)
			}
			if got == nil || got.ID != "2" {
				t.Fatalf("expected agent id 2, got %+v", got)
			}
			// No match returns nil, nil.
			got, err = c.FindByName(context.Background(), "gamma")
			if err != nil || got != nil {
				t.Fatalf("expected (nil,nil), got (%+v,%v)", got, err)
			}
		})
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n*len(s))
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// STORY-E6-S8: GetCurrentPolicy parses the {status,data:PolicyEntity|null}
// envelope, sends the org key on X-API-Key with the read:agent_policy path, and
// extracts config.policy_builder / raw-rego presence.
func TestGetCurrentPolicy_BuilderConfig(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":200,"data":{"id":"pol-1","updated_at":"2026-07-15T00:00:00Z","rego_code":"package x","config":{"path":"org/o/policy_1","policy_builder":{"version":1,"rules":[]}}}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "obx_key_"+repeat("f", 48), "openbox-cli")
	p, err := c.GetCurrentPolicy(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("GetCurrentPolicy: %v", err)
	}
	if gotPath != "/agent/agent-1/policies/current" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth == "" {
		t.Error("org key must go on X-API-Key")
	}
	if p == nil || p.ID != "pol-1" || p.UpdatedAt != "2026-07-15T00:00:00Z" {
		t.Fatalf("policy pin not parsed: %+v", p)
	}
	if len(p.PolicyBuilder) == 0 {
		t.Errorf("policy_builder not extracted: %+v", p)
	}
	if p.HasRawRego {
		t.Errorf("HasRawRego must be false when policy_builder is present")
	}
}

func TestGetCurrentPolicy_NullDataAndRawRego(t *testing.T) {
	// data:null → (nil,nil): no current policy.
	nullSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":200,"data":null}`)
	}))
	defer nullSrv.Close()
	if p, err := New(nullSrv.URL, "obx_key_x", "").GetCurrentPolicy(context.Background(), "a"); err != nil || p != nil {
		t.Fatalf("null data = (%+v,%v), want (nil,nil)", p, err)
	}

	// raw rego, no policy_builder → HasRawRego true.
	rawSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":200,"data":{"id":"pol-2","updated_at":"t","rego_code":"package x\nallow { true }","config":{"path":"p"}}}`)
	}))
	defer rawSrv.Close()
	p, err := New(rawSrv.URL, "obx_key_x", "").GetCurrentPolicy(context.Background(), "a")
	if err != nil {
		t.Fatalf("raw rego: %v", err)
	}
	if p == nil || !p.HasRawRego || len(p.PolicyBuilder) != 0 {
		t.Fatalf("raw-rego policy = %+v, want HasRawRego=true, no builder", p)
	}
}

func TestGetCurrentPolicy_APIErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `forbidden`)
	}))
	defer srv.Close()
	_, err := New(srv.URL, "obx_key_x", "").GetCurrentPolicy(context.Background(), "a")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
		t.Fatalf("want *APIError 403, got %v", err)
	}
}
