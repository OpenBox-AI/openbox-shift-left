package workloadauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
)

const testSDKVersion = "openbox-shift-left-go-test"

// validBootstrapDoc is a canonical document any single test then mutates one
// field of, so TestBootstrapRejectsEveryNonCanonicalDocument proves each
// validation rule independently rather than testing several at once.
func validBootstrapDoc() map[string]any {
	return map[string]any{
		"bootstrap_version":  3,
		"contract_version":   3,
		"issuer":             "https://identity.node.lat/realms/openbox",
		"token_endpoint":     "https://identity.node.lat/realms/openbox/protocol/openid-connect/token",
		"audience":           "openbox-core",
		"client_id":          "workload-client-1",
		"service_account_id": "5f0f8a5e-4b6a-4d3a-9a1b-1a2b3c4d5e6f",
		"activation_version": "5f0f8a5e-4b6a-4d3a-9a1b-1a2b3c4d5e70",
		"identity_source":    "openbox",
		"kid":                "kid-1",
	}
}

func bootstrapServer(t *testing.T, status int, doc map[string]any) *memhttptest.Server {
	t.Helper()
	return memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(doc)
	}))
}

func TestBootstrapRequestCarriesAPIKeyOnly(t *testing.T) {
	var gotAuth, gotSDK, gotUA, gotAccept, gotMethod, gotPath, gotWorkloadHeader string
	var bodyLen int
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSDK = r.Header.Get("X-OpenBox-SDK-Version")
		gotUA = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotWorkloadHeader = r.Header.Get("X-OpenBox-Workload-Token")
		buf := make([]byte, 1)
		n, _ := r.Body.Read(buf)
		bodyLen = n

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(validBootstrapDoc())
	}))

	_, err := Bootstrap(context.Background(), srv.Client(), srv.URL, "obx_testkey", testSDKVersion)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/api/v3/auth/bootstrap" {
		t.Errorf("path = %q, want /api/v3/auth/bootstrap", gotPath)
	}
	if gotAuth != "Bearer obx_testkey" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotSDK == "" {
		t.Error("X-OpenBox-SDK-Version was empty")
	}
	if gotUA == "" {
		t.Error("User-Agent was empty")
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", gotAccept)
	}
	if gotWorkloadHeader != "" {
		t.Errorf("bootstrap must carry no workload header, got %q", gotWorkloadHeader)
	}
	if bodyLen != 0 {
		t.Errorf("bootstrap GET must carry no body, read %d byte(s)", bodyLen)
	}
}

func TestBootstrapRejectsEveryNonCanonicalDocument(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"bootstrap_version != 3", func(d map[string]any) { d["bootstrap_version"] = 2 }},
		{"contract_version != 3", func(d map[string]any) { d["contract_version"] = 2 }},
		{"http non-loopback issuer", func(d map[string]any) {
			d["issuer"] = "http://identity.node.lat/realms/openbox"
			d["token_endpoint"] = "http://identity.node.lat/realms/openbox/protocol/openid-connect/token"
		}},
		{"issuer has query", func(d map[string]any) { d["issuer"] = "https://identity.node.lat/realms/openbox?x=1" }},
		{"issuer has fragment", func(d map[string]any) { d["issuer"] = "https://identity.node.lat/realms/openbox#frag" }},
		{"issuer has userinfo", func(d map[string]any) { d["issuer"] = "https://user:pass@identity.node.lat/realms/openbox" }},
		{"token_endpoint != issuer+suffix", func(d map[string]any) { d["token_endpoint"] = "https://identity.node.lat/realms/openbox/wrong-path" }},
		{"empty client_id", func(d map[string]any) { d["client_id"] = "" }},
		{"empty kid", func(d map[string]any) { d["kid"] = "" }},
		{"empty audience", func(d map[string]any) { d["audience"] = "" }},
		{"uppercase service_account_id", func(d map[string]any) { d["service_account_id"] = "5F0F8A5E-4B6A-4D3A-9A1B-1A2B3C4D5E6F" }},
		{"non-uuid service_account_id", func(d map[string]any) { d["service_account_id"] = "not-a-uuid" }},
		{"uppercase activation_version", func(d map[string]any) { d["activation_version"] = "5F0F8A5E-4B6A-4D3A-9A1B-1A2B3C4D5E70" }},
		{"non-uuid activation_version", func(d map[string]any) { d["activation_version"] = "not-a-uuid" }},
		{"unknown identity_source", func(d map[string]any) { d["identity_source"] = "azure" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := validBootstrapDoc()
			tc.mutate(doc)
			srv := bootstrapServer(t, http.StatusOK, doc)
			_, err := Bootstrap(context.Background(), srv.Client(), srv.URL, "obx_testkey", testSDKVersion)
			if err == nil {
				t.Fatalf("expected %s to be rejected", tc.name)
			}
		})
	}
}

func TestBootstrapAcceptsTheCanonicalDocument(t *testing.T) {
	srv := bootstrapServer(t, http.StatusOK, validBootstrapDoc())
	doc, err := Bootstrap(context.Background(), srv.Client(), srv.URL, "obx_testkey", testSDKVersion)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if doc.ClientID != "workload-client-1" {
		t.Errorf("ClientID = %q", doc.ClientID)
	}
	if doc.TokenEndpoint != "https://identity.node.lat/realms/openbox/protocol/openid-connect/token" {
		t.Errorf("TokenEndpoint = %q", doc.TokenEndpoint)
	}
}

func TestBootstrapOutageDoesNotFallBack(t *testing.T) {
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"reason_code":"verifier_unavailable"}`))
	}))
	_, err := Bootstrap(context.Background(), srv.Client(), srv.URL, "obx_testkey", testSDKVersion)
	if err == nil {
		t.Fatal("expected an error on 503")
	}
	if !asWorkloadError(t, err).Transient {
		t.Errorf("503 must be classified transient, got %+v", err)
	}
	if asWorkloadError(t, err).Status != http.StatusServiceUnavailable {
		t.Errorf("Status = %d, want 503", asWorkloadError(t, err).Status)
	}
}

func TestUnavailableIdentityBootstrapIsClassifiedNotTransient(t *testing.T) {
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"reason_code":"workload_identity_unavailable"}`))
	}))
	_, err := Bootstrap(context.Background(), srv.Client(), srv.URL, "obx_testkey", testSDKVersion)
	if err == nil {
		t.Fatal("expected an error on 409")
	}
	if asWorkloadError(t, err).Transient {
		t.Errorf("409 must not be classified transient, got %+v", err)
	}
	if asWorkloadError(t, err).Reason != "workload_identity_unavailable" {
		t.Errorf("Reason = %q, want workload_identity_unavailable", asWorkloadError(t, err).Reason)
	}
	if asWorkloadError(t, err).Stage != StageBootstrap {
		t.Errorf("Stage = %q, want bootstrap", asWorkloadError(t, err).Stage)
	}
}

func TestResponsesAreBounded(t *testing.T) {
	huge := strings.Repeat("x", 2<<20) // 2 MiB
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"padding":%q`, huge) // deliberately unterminated past the cap
	}))
	_, err := Bootstrap(context.Background(), srv.Client(), srv.URL, "obx_testkey", testSDKVersion)
	if err == nil {
		t.Fatal("expected a truncated 2 MiB body to fail to parse")
	}
}
