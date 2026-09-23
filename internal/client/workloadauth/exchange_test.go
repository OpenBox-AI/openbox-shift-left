package workloadauth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
)

func exchangeDoc(endpoint string) *BootstrapDocument {
	return &BootstrapDocument{
		TokenEndpoint: endpoint,
		ClientID:      "workload-client-1",
		Kid:           "kid-1",
	}
}

func TestExchangeFormIsExactAndUnauthenticated(t *testing.T) {
	var gotMethod, gotAuth, gotContentType string
	var form map[string][]string

	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		form = map[string][]string(r.PostForm)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "atk_test",
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))

	doc := exchangeDoc(srv.URL + "/protocol/openid-connect/token")
	res, err := Exchange(context.Background(), srv.Client(), doc, "assertion.jwt.value")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if res.AccessToken != "atk_test" {
		t.Errorf("AccessToken = %q", res.AccessToken)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotAuth != "" {
		t.Errorf("exchange must carry no Authorization header, got %q", gotAuth)
	}
	if !strings.Contains(gotContentType, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	if got := form["grant_type"]; len(got) != 1 || got[0] != "client_credentials" {
		t.Errorf("grant_type = %v", got)
	}
	if got := form["client_id"]; len(got) != 1 || got[0] != doc.ClientID {
		t.Errorf("client_id = %v", got)
	}
	if got := form["client_assertion_type"]; len(got) != 1 || got[0] != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" {
		t.Errorf("client_assertion_type = %v", got)
	}
	if got := form["client_assertion"]; len(got) != 1 || got[0] != "assertion.jwt.value" {
		t.Errorf("client_assertion = %v", got)
	}
}

func TestExchangeRejectsNonBearerAndShortExpiry(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{"non-Bearer token_type", map[string]any{"access_token": "atk", "token_type": "MAC", "expires_in": 300}},
		{"expires_in too short", map[string]any{"access_token": "atk", "token_type": "Bearer", "expires_in": 30}},
		{"expires_in is a bool", map[string]any{"access_token": "atk", "token_type": "Bearer", "expires_in": true}},
		{"empty access_token", map[string]any{"access_token": "", "token_type": "Bearer", "expires_in": 300}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(tc.body)
			}))
			doc := exchangeDoc(srv.URL + "/protocol/openid-connect/token")
			_, err := Exchange(context.Background(), srv.Client(), doc, "assertion.jwt.value")
			if err == nil {
				t.Fatalf("expected %s to be rejected", tc.name)
			}
		})
	}
}

func TestExchangeAcceptsCaseInsensitiveBearer(t *testing.T) {
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "atk",
			"token_type":   "bearer",
			"expires_in":   300,
		})
	}))
	doc := exchangeDoc(srv.URL + "/protocol/openid-connect/token")
	res, err := Exchange(context.Background(), srv.Client(), doc, "assertion.jwt.value")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if res.AccessToken != "atk" {
		t.Errorf("AccessToken = %q", res.AccessToken)
	}
}

func TestExchangeRejectionIsPermanentAndNamesTheClock(t *testing.T) {
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":             "invalid_client",
			"error_description": "assertion rejected",
		})
	}))
	doc := exchangeDoc(srv.URL + "/protocol/openid-connect/token")
	_, err := Exchange(context.Background(), srv.Client(), doc, "assertion.jwt.value")
	if err == nil {
		t.Fatal("expected an error on 401")
	}
	if err.Transient {
		t.Errorf("401 must not be classified transient, got %+v", err)
	}
	if !strings.Contains(err.Error(), "NTP") {
		t.Errorf("401 exchange error should name the host clock, got: %v", err)
	}
	if err.Reason != "invalid_client" {
		t.Errorf("Reason = %q, want invalid_client", err.Reason)
	}
}

func TestExchange5xxIsTransient(t *testing.T) {
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"upstream_failure"}`))
	}))
	doc := exchangeDoc(srv.URL + "/protocol/openid-connect/token")
	_, err := Exchange(context.Background(), srv.Client(), doc, "assertion.jwt.value")
	if err == nil {
		t.Fatal("expected an error on 502")
	}
	if !err.Transient {
		t.Errorf("502 must be classified transient, got %+v", err)
	}
}
