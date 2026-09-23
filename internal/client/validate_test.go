package client

import (
	"context"
	"io"
	"net/http"

	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
)

// TestValidate_HappyPath_AuthenticatedGET drives the real Validate → workload
// GET path against a core mirror that checks the envelope (obx_ key + workload
// bearer) exactly as ValidateDetailed builds it.
func TestValidate_HappyPath_AuthenticatedGET(t *testing.T) {
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != AuthValidatePath {
			t.Errorf("path = %q, want %q", r.URL.Path, AuthValidatePath)
		}
		if got := r.Header.Get(headerAuthorization); got != "Bearer "+testAPIKey {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get(headerWorkloadToken); got != testWorkloadToken {
			t.Errorf("workload token header = %q, want %q", got, testWorkloadToken)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"valid":true,"active":true,"agent_id":"a","agent_name":"test-agent"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv.URL, false)
	if err := c.Validate(context.Background()); err != nil {
		t.Fatalf("Validate returned error on a valid authenticated GET: %v", err)
	}
}

// TestValidate_MapsNon200 covers the reasons a reachability check must render
// as an actionable ✗: a flat runtime 401 (core carries no reason code on
// that route) and the forward-compat bootstrap reason-code envelope.
func TestValidate_MapsNon200(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string // substring the mapped diagnostic must contain
	}{
		{"401 invalid token", 401, `{"code":401,"message":"invalid token"}`, "run `openbox doctor`"},
		{"401 no message", 401, `{"code":401}`, "run `openbox doctor`"},
		{"500 internal error", 500, `{"code":500,"message":"internal server error"}`, "internal server error"},
		{"forward-compat agent_inactive", 403, `{"reason_code":"agent_inactive"}`, "deactivated by an operator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fixedRespServer(t, tc.status, tc.body)
			c, _ := newTestClient(t, srv.URL, false)

			err := c.Validate(context.Background())
			if err == nil {
				t.Fatalf("Validate returned nil for HTTP %d", tc.status)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate error = %q, want substring %q", err.Error(), tc.want)
			}
			ve, ok := AsValidateError(err)
			if !ok {
				t.Fatalf("error is not a *ValidateError: %v", err)
			}
			if ve.Status != tc.status {
				t.Errorf("ValidateError.Status = %d, want %d", ve.Status, tc.status)
			}
			if strings.Contains(err.Error(), testAPIKey) || strings.Contains(err.Error(), testWorkloadToken) {
				t.Error("INV-1 violation: secret material leaked into the validate diagnostic")
			}
		})
	}
}

// TestValidate_TransportFailureIsClearError: an unreachable core is a clear ✗
// (not a hang, not a *ValidateError) so the CLI can distinguish "couldn't
// reach core" from "core said no".
func TestValidate_TransportFailureIsClearError(t *testing.T) {
	c, _ := newTestClient(t, "http://127.0.0.1:1", false) // closed port
	err := c.Validate(context.Background())
	if err == nil {
		t.Fatal("Validate returned nil for an unreachable core")
	}
	if !strings.Contains(err.Error(), "could not reach core") {
		t.Errorf("transport error = %q, want it to name the unreachable core", err.Error())
	}
	if _, ok := AsValidateError(err); ok {
		t.Error("a transport failure must not be a *ValidateError (no HTTP status)")
	}
}
