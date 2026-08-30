package client

import (
	"context"
	"io"
	"net/http"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"strings"
	"testing"
)

// TestValidate_HappyPath_SignedGET drives the real Validate → signed GET path
// against a core mirror that verifies the AIP signature exactly as openbox-core
// would (empty-body SHA, canonical GET string, Ed25519 verify). A 200 → nil.
func TestValidate_HappyPath_SignedGET(t *testing.T) {
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
		body, _ := io.ReadAll(r.Body)
		// The GET carries no body; core hashes an empty body and rebuilds the
		// canonical GET string from the headers. This is exactly core's check.
		if err := verifyLikeCore(pub(t), r.Method, r.URL.Path, body, r.Header); err != nil {
			t.Errorf("core-mirror rejected the signed GET: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"valid":true,"active":true,"agent_id":"a","environment":"test","message":"ok"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv.URL, false)
	if err := c.Validate(context.Background()); err != nil {
		t.Fatalf("Validate returned error on a valid signed GET: %v", err)
	}
}

// TestValidate_MapsNon200 covers the reasons a `dev verify` must render as an
// actionable ✗: the stock-core collapsed 401/500 responses (reason codes are NOT
// in the body) and the forward-compat reason-code envelope. Each maps through the
// shared SL-10 table, carries the HTTP status via *ValidateError, and never leaks
// a secret (INV-1).
func TestValidate_MapsNon200(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string // substring the mapped diagnostic must contain
	}{
		{"401 invalid token", 401, `{"code":401,"message":"invalid token"}`, "identity rejected"},
		{"401 missing auth", 401, `{"code":401,"message":"missing authorization token"}`, "no Authorization bearer"},
		// verifier_not_configured collapses to a 500 whose message names the verifier
		// (verified openbox-core) — map it to the fix, not a transient shrug.
		{"500 verifier unavailable", 500, `{"code":500,"message":"internal server error: agent DID identity verifier unavailable"}`, "signing_required=false"},
		{"forward-compat signature_invalid", 401, `{"reason_code":"signature_invalid"}`, "signed bytes were rejected"},
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
			// INV-1: the key/seed must never appear in the diagnostic.
			if strings.Contains(err.Error(), testAPIKey) || strings.Contains(err.Error(), testPrivateKeyB64) {
				t.Error("INV-1 violation: secret material leaked into the validate diagnostic")
			}
		})
	}
}

// TestValidate_TransportFailureIsClearError: an unreachable core is a clear ✗
// (not a hang, not a *ValidateError) so the CLI can distinguish "couldn't reach
// core" from "core said no".
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
