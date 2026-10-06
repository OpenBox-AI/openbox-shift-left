package backend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"testing"

	"github.com/google/uuid"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/aivss"
)

func TestCreateParsesResponseAndSendsContract(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotClient, gotContentType string
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/agent/create" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("X-API-Key")
		gotClient = r.Header.Get("x-openbox-client")
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"agent":{"id":"a-1","agent_name":"dev","tier":"Tier 2","trust_score":0.81},"token":"obx_test_`+repeat("a", 48)+`","identity":{"kid":"kid-x"}}}`)
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

	if gotAuth == "" {
		t.Error("expected X-API-Key header to be set")
	}
	if gotClient != "" {
		t.Errorf("x-openbox-client should be empty on the API-key path, got %q", gotClient)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	if gotBody["agent_type"] != "developer" {
		t.Errorf("agent_type = %v, want developer", gotBody["agent_type"])
	}
	if gotBody["icon"] == "" || gotBody["icon"] == nil {
		t.Error("icon must be non-empty (backend DTO @IsNotEmpty)")
	}
	if _, ok := gotBody["aivss_config"]; !ok {
		t.Error("aivss_config must be present")
	}
	if reg.APIKey == "" || reg.Identity.Kid != "kid-x" {
		t.Errorf("bad registration parse: %+v", reg)
	}
	if reg.AgentID != "a-1" || reg.Tier != "Tier 2" {
		t.Errorf("bad agent fields: %+v", reg)
	}
}

func TestCreateBearerPathSetsClientHeader(t *testing.T) {
	var gotAuth, gotClient string
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotClient = r.Header.Get("x-openbox-client")
		_, _ = io.WriteString(w, `{"data":{"agent":{"id":"a"},"token":"t","identity":{"kid":"k"}}}`)
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

func TestCreateAPIError(t *testing.T) {
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// TestCreateSendsIdentityVerificationVerbatim the
// identity_verification block reaches the wire with exactly the requested
// keys, and only the public JWK -- never a private RSA parameter.
func TestCreateSendsIdentityVerificationVerbatim(t *testing.T) {
	var gotBody map[string]any
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"agent":{"id":"a"},"token":"t","identity":{}}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "obx_key_x", "cli")
	_, err := c.Create(context.Background(), CreateAgentRequest{
		AgentName:   "dev",
		AgentType:   "developer",
		Icon:        "🧑‍💻",
		AivssConfig: aivss.DefaultDeveloperProfile(),
		IdentityVerification: &IdentityVerification{
			Method:     "keycloak_workload",
			Mode:       "generate",
			SourceType: "openbox",
			PublicJWK: map[string]string{
				"kty": "RSA",
				"n":   "modulus-b64url",
				"e":   "AQAB",
				"kid": "kid-1",
				"alg": "RS256",
				"use": "sig",
			},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	iv, ok := gotBody["identity_verification"].(map[string]any)
	if !ok {
		t.Fatalf("identity_verification missing or wrong shape in %+v", gotBody)
	}
	if iv["method"] != "keycloak_workload" || iv["mode"] != "generate" || iv["source_type"] != "openbox" {
		t.Errorf("identity_verification top-level fields = %+v", iv)
	}
	jwk, ok := iv["public_jwk"].(map[string]any)
	if !ok {
		t.Fatalf("public_jwk missing or wrong shape in %+v", iv)
	}
	for _, want := range []string{"kty", "n", "e", "kid", "alg", "use"} {
		if _, present := jwk[want]; !present {
			t.Errorf("public_jwk missing %q: %+v", want, jwk)
		}
	}
	for _, forbidden := range []string{"d", "p", "q", "dp", "dq", "qi"} {
		if _, present := jwk[forbidden]; present {
			t.Errorf("public_jwk must never carry the private parameter %q", forbidden)
		}
	}
}

// TestCreateParsesWorkloadIdentity a response shaped
// like agent-registration-identity.service.ts:471-484 parses into
// Registration.Identity, non-secret fields only.
func TestCreateParsesWorkloadIdentity(t *testing.T) {
	workloadID := uuid.NewString()
	credentialID := uuid.NewString()
	serviceAccountID := uuid.NewString()
	clientID := "openbox-agent-" + uuid.NewString()
	kid := uuid.NewString()

	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"agent":{"id":"a-1","agent_name":"dev"},"token":"obx_test_`+repeat("a", 48)+`",`+
			`"identity":{"method":"keycloak_workload","source_type":"openbox",`+
			`"workload_identity_id":"`+workloadID+`","credential_id":"`+credentialID+`",`+
			`"service_account_id":"`+serviceAccountID+`","client_id":"`+clientID+`","kid":"`+kid+`",`+
			`"token_endpoint":"https://identity.example.com/realms/openbox/protocol/openid-connect/token",`+
			`"audience":"openbox-core","private_key_available_from_openbox":false}}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "obx_key_x", "cli")
	reg, err := c.Create(context.Background(), CreateAgentRequest{AgentName: "dev"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := WorkloadIdentityInfo{
		Method:                         "keycloak_workload",
		SourceType:                     "openbox",
		WorkloadIdentityID:             workloadID,
		CredentialID:                   credentialID,
		ServiceAccountID:               serviceAccountID,
		ClientID:                       clientID,
		Kid:                            kid,
		TokenEndpoint:                  "https://identity.example.com/realms/openbox/protocol/openid-connect/token",
		Audience:                       "openbox-core",
		PrivateKeyAvailableFromOpenBox: false,
	}
	if reg.Identity != want {
		t.Errorf("Registration.Identity = %+v, want %+v", reg.Identity, want)
	}
}

// TestCreateOmitsIdentityVerificationWhenNil pins the v1 request byte-
// identical while a caller leaves IdentityVerification unset.
func TestCreateOmitsIdentityVerificationWhenNil(t *testing.T) {
	var raw []byte
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"data":{"agent":{"id":"a"},"token":"t","identity":{}}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "obx_key_x", "cli")
	if _, err := c.Create(context.Background(), CreateAgentRequest{AgentName: "dev", AgentType: "developer"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if strings.Contains(string(raw), "identity_verification") {
		t.Errorf("request body must omit identity_verification when nil, got %s", raw)
	}
}

// TestClassifyCreateConflict the create 409s carry no reason_code, so the
// classifier reads the message string. The ExternalIdP primary match is the
// source-mismatch string (agent-registration-identity.service.ts:198) a
// non-OpenBox org actually returns for source_type=openbox;
// "New identities must be created…" is the secondary match.
func TestClassifyCreateConflict(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want CreateConflict
	}{
		{
			name: "idp not initialized",
			err:  &APIError{StatusCode: 409, Body: `{"message":"The organization identity provider must be initialized before creating a workload-authenticated agent"}`},
			want: IdPNotInitialized,
		},
		{
			name: "external idp primary: source mismatch (:198)",
			err:  &APIError{StatusCode: 409, Body: `{"message":"The selected identity does not belong to the active identity provider"}`},
			want: ExternalIdP,
		},
		{
			name: "external idp secondary: generate on external provider",
			err:  &APIError{StatusCode: 409, Body: `{"message":"New identities must be created in the active external provider"}`},
			want: ExternalIdP,
		},
		{
			name: "idp changed mid-registration, retryable",
			err:  &APIError{StatusCode: 409, Body: `{"message":"The active identity provider changed during agent registration"}`},
			want: IdPChangedRetry,
		},
		{
			name: "unknown 409 falls through",
			err:  &APIError{StatusCode: 409, Body: `{"message":"The selected workload identity is not active, verified, and eligible"}`},
			want: Other409,
		},
		{
			name: "400 is not a create conflict",
			err:  &APIError{StatusCode: 400, Body: `{"message":"Agent with name \"dev\" already exists in this organization"}`},
			want: None,
		},
		{
			name: "non-APIError is not a create conflict",
			err:  errors.New("boom"),
			want: None,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyCreateConflict(tt.err); got != tt.want {
				t.Errorf("ClassifyCreateConflict(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestIsDuplicateNameConflict this race carries no code and no 409: the
// backend answers HTTP 400 for it (agent.service.ts:978-981), so it needs its
// own classifier rather than folding into ClassifyCreateConflict.
func TestIsDuplicateNameConflict(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "400 duplicate name, exact backend wording",
			err:  &APIError{StatusCode: 400, Body: `{"message":"Agent with name \"dev-x-a1b2c3\" already exists in this organization"}`},
			want: true,
		},
		{
			name: "400 duplicate name, case-insensitive match",
			err:  &APIError{StatusCode: 400, Body: `{"MESSAGE":"AGENT WITH NAME already EXISTS IN THIS ORGANIZATION"}`},
			want: true,
		},
		{
			name: "400 for an unrelated reason",
			err:  &APIError{StatusCode: 400, Body: `{"message":"AIVSS config is required"}`},
			want: false,
		},
		{
			name: "409 conflict is not this classifier's concern",
			err:  &APIError{StatusCode: 409, Body: `{"message":"Agent with name \"dev-x\" already exists in this organization"}`},
			want: false,
		},
		{
			name: "non-APIError",
			err:  errors.New("boom"),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsDuplicateNameConflict(tt.err); got != tt.want {
				t.Errorf("IsDuplicateNameConflict(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestFindByName(t *testing.T) {
	cases := map[string]string{
		"bare array": `{"data":[{"id":"1","agent_name":"alpha"},{"id":"2","agent_name":"Beta"}]}`,
		"paged":      `{"data":{"items":[{"id":"1","agent_name":"alpha"},{"id":"2","agent_name":"Beta"}]}}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/agent/list" {
					t.Errorf("path = %s", r.URL.Path)
				}
				if r.URL.Query().Get("all") != "true" {
					t.Errorf("query = %q, want all=true", r.URL.RawQuery)
				}
				_, _ = io.WriteString(w, payload)
			}))
			defer srv.Close()
			c := New(srv.URL, "obx_key_x", "cli")

			got, err := c.FindByName(context.Background(), "beta")
			if err != nil {
				t.Fatalf("FindByName: %v", err)
			}
			if got == nil || got.ID != "2" {
				t.Fatalf("expected agent id 2, got %+v", got)
			}
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

// TestGetCurrentPolicy_BuilderConfig: GetCurrentPolicy parses the
// {status,data:PolicyEntity|null} envelope, sends the org key on X-API-Key
// with the read:agent_policy path, and extracts config.policy_builder / raw-
// rego presence.
func TestGetCurrentPolicy_BuilderConfig(t *testing.T) {
	var gotPath, gotAuth string
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	nullSrv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":200,"data":null}`)
	}))
	defer nullSrv.Close()
	if p, err := New(nullSrv.URL, "obx_key_x", "").GetCurrentPolicy(context.Background(), "a"); err != nil || p != nil {
		t.Fatalf("null data = (%+v,%v), want (nil,nil)", p, err)
	}

	rawSrv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// TestFindByName_UnrecognizedShapeErrors an unreadable list shape must be an
// error, not an empty list.
func TestFindByName_UnrecognizedShapeErrors(t *testing.T) {
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"agents":[{"id":"1","agent_name":"alpha"}]}}`)
	}))
	defer srv.Close()

	got, err := New(srv.URL, "obx_key_x", "cli").FindByName(context.Background(), "alpha")
	if err == nil {
		t.Fatalf("expected an error on an unrecognized list shape, got (%+v, nil)", got)
	}
	if got != nil {
		t.Errorf("no agent should be returned alongside the error, got %+v", got)
	}
}

// TestFindByName_EmptyDataIsNotAnError an empty data field is a legitimate "no
// agents", not a parse failure.
func TestFindByName_EmptyDataIsNotAnError(t *testing.T) {
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()

	got, err := New(srv.URL, "obx_key_x", "cli").FindByName(context.Background(), "alpha")
	if err != nil || got != nil {
		t.Fatalf("expected (nil, nil) for an empty list, got (%+v, %v)", got, err)
	}
}
