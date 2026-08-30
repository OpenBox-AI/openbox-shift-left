package backend

import (
	"context"
	"net/http"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"strings"
	"testing"
)

func rotateServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *memhttptest.Server {
	t.Helper()
	srv := memhttptest.NewServer(t, http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	return srv
}

func TestRotateAPIKeyHappyPath(t *testing.T) {
	srv := rotateServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/agent/agent-1/rotate-api-key" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"token":"obx_new_key"}`))
	})
	got, err := New(srv.URL, "obx_key_x", "openbox-cli").RotateAPIKey(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("RotateAPIKey: %v", err)
	}
	if got != "obx_new_key" {
		t.Errorf("token = %q", got)
	}
}

// TestRotateToleratesTheDataEnvelope the control plane uses both a bare body
// and a {status,data:{…}} envelope across endpoints, so the decoder must find
// the field either way rather than assume a depth.
func TestRotateToleratesTheDataEnvelope(t *testing.T) {
	srv := rotateServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "rotate-api-key"):
			_, _ = w.Write([]byte(`{"status":200,"data":{"token":"obx_enveloped"}}`))
		default:
			_, _ = w.Write([]byte(`{"status":200,"data":{"did":"did:aip:same","privateKey":"ZW52"}}`))
		}
	})
	c := New(srv.URL, "obx_key_x", "openbox-cli")
	tok, err := c.RotateAPIKey(context.Background(), "agent-1")
	if err != nil || tok != "obx_enveloped" {
		t.Fatalf("token = %q, err = %v", tok, err)
	}
	did, key, err := c.RotateIdentity(context.Background(), "agent-1")
	if err != nil || did != "did:aip:same" || key != "ZW52" {
		t.Fatalf("did = %q key = %q err = %v", did, key, err)
	}
}

// TestRotateAPIKeyMissingTokenFailsLoudly a 2xx that omits the credential must
// fail.
func TestRotateAPIKeyMissingTokenFailsLoudly(t *testing.T) {
	srv := rotateServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":200,"data":{}}`))
	})
	_, err := New(srv.URL, "obx_key_x", "openbox-cli").RotateAPIKey(context.Background(), "agent-1")
	if err == nil {
		t.Fatal("a 2xx with no token must be an error")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error should name the missing field, got %v", err)
	}
}

// TestRotateIdentityMissingPrivateKeyFailsLoudly the guard that matters most:
// AgentIdentityResponseDto does not declare privateKey, so a response
// serializer added upstream would strip it and every rotation would silently
// return nothing usable.
func TestRotateIdentityMissingPrivateKeyFailsLoudly(t *testing.T) {
	srv := rotateServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"did":"did:aip:same","key_id":"k","public_key":"p"}`))
	})
	_, _, err := New(srv.URL, "obx_key_x", "openbox-cli").RotateIdentity(context.Background(), "agent-1")
	if err == nil {
		t.Fatal("a 2xx with no privateKey must be an error")
	}
	if !strings.Contains(err.Error(), "privateKey") {
		t.Errorf("error should name the missing field, got %v", err)
	}
}

func TestRotateIdentityMissingDIDFailsLoudly(t *testing.T) {
	srv := rotateServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"privateKey":"ZW52"}`))
	})
	_, _, err := New(srv.URL, "obx_key_x", "openbox-cli").RotateIdentity(context.Background(), "agent-1")
	if err == nil {
		t.Fatal("a private key with no DID must be an error")
	}
}

func TestRotateErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		wantText string
	}{
		{
			name: "401 names the wrong credential type", status: http.StatusUnauthorized,
			body: `{"message":"Invalid API key"}`, wantText: "obx_key_",
		},
		{
			name: "403 names the missing permission", status: http.StatusForbidden,
			body: `{"message":"Forbidden"}`, wantText: "update:agent",
		},
		{
			name: "404 not-provisioned reads differently", status: http.StatusNotFound,
			body: `{"message":"Agent identity has not been provisioned"}`, wantText: "no signing identity provisioned",
		},
		{
			name: "404 unknown agent", status: http.StatusNotFound,
			body: `{"message":"Agent not found"}`, wantText: "no agent agent-1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := rotateServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, _, err := New(srv.URL, "obx_key_x", "openbox-cli").RotateIdentity(context.Background(), "agent-1")
			if err == nil {
				t.Fatalf("HTTP %d should be an error", tc.status)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantText)
			}
		})
	}
}

// TestTheTwo404sDoNotReadAlike the two 404s must not be confusable. Pinned as
// a pair because the whole point of the body check is that the status code
// alone cannot tell them apart.
func TestTheTwo404sDoNotReadAlike(t *testing.T) {
	msg := func(body string) string {
		srv := rotateServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(body))
		})
		_, _, err := New(srv.URL, "obx_key_x", "openbox-cli").RotateIdentity(context.Background(), "agent-1")
		return err.Error()
	}
	notProvisioned := msg(`{"message":"Agent identity has not been provisioned"}`)
	unknown := msg(`{"message":"Agent not found"}`)
	if notProvisioned == unknown {
		t.Fatal("the two 404 cases produce identical messages")
	}
}

// TestRotateUsesXAPIKeyForAnOrgKeyAndBearerOtherwise getting the auth header
// wrong yields a bare 401 with no clue why, so it is pinned: an obx_key_ org
// key goes in X-API-Key, anything else as a Bearer JWT.
func TestRotateUsesXAPIKeyForAnOrgKeyAndBearerOtherwise(t *testing.T) {
	for _, tc := range []struct {
		credential, wantHeader, wantValue string
	}{
		{"obx_key_orgkey", "X-Api-Key", "obx_key_orgkey"},
		{"header.payload.sig", "Authorization", "Bearer header.payload.sig"},
	} {
		var gotHeader, gotValue string
		srv := rotateServer(t, func(w http.ResponseWriter, r *http.Request) {
			if v := r.Header.Get("X-API-Key"); v != "" {
				gotHeader, gotValue = "X-Api-Key", v
			} else if v := r.Header.Get("Authorization"); v != "" {
				gotHeader, gotValue = "Authorization", v
			}
			_, _ = w.Write([]byte(`{"token":"obx_new"}`))
		})
		if _, err := New(srv.URL, tc.credential, "openbox-cli").RotateAPIKey(context.Background(), "agent-1"); err != nil {
			t.Fatal(err)
		}
		if gotHeader != tc.wantHeader || gotValue != tc.wantValue {
			t.Errorf("credential %q sent %s: %q, want %s: %q", tc.credential, gotHeader, gotValue, tc.wantHeader, tc.wantValue)
		}
	}
}

func TestRotateRejectsAnEmptyAgentID(t *testing.T) {
	c := New("https://unused.example", "obx_key_x", "openbox-cli")
	if _, err := c.RotateAPIKey(context.Background(), "  "); err == nil {
		t.Error("RotateAPIKey accepted a blank agent id")
	}
	if _, _, err := c.RotateIdentity(context.Background(), ""); err == nil {
		t.Error("RotateIdentity accepted a blank agent id")
	}
}
