package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDiagnose_ForwardCompatReasonCodes feeds each SDK reason code as a string
// body field (the shape a future EXT-core would emit) and asserts the mapped,
// actionable guidance. Covers all six codes plus the reason_code/code/reason key
// order and the unrecognized-code fallback.
func TestDiagnose_ForwardCompatReasonCodes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string // substring the guidance must contain
	}{
		{"signature_invalid", 401, `{"reason_code":"signature_invalid","message":"x"}`, "signed bytes were rejected"},
		{"nonce_replayed", 401, `{"reason_code":"nonce_replayed"}`, "buffered event was re-sent"},
		{"did_agent_mismatch", 401, `{"reason_code":"did_agent_mismatch"}`, "does not match"},
		{"verifier_not_configured", 500, `{"reason_code":"verifier_not_configured"}`, "no KMS verifier"},
		{"timestamp_outside_window", 401, `{"reason_code":"timestamp_outside_window"}`, "±300s"},
		{"timestamp_skew alias", 401, `{"reason_code":"timestamp_skew"}`, "±300s"},
		// key order: `reason` is read when reason_code/code are absent.
		{"legacy reason key", 401, `{"reason":"nonce_replayed"}`, "buffered event was re-sent"},
		// a STRING `code` is honored (config.py order); stock core's INT code is not.
		{"string code key", 401, `{"code":"did_agent_mismatch"}`, "does not match"},
		// unrecognized reason → raw code + status, no guess, no crash.
		{"unknown reason", 401, `{"reason_code":"teapot"}`, "unrecognized reason code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := diagnose(tc.status, tc.body)
			if !strings.Contains(got, tc.want) {
				t.Errorf("diagnose(%d, %s) = %q, want substring %q", tc.status, tc.body, got, tc.want)
			}
		})
	}
	// the unknown-reason line must echo the raw code and the status (no guess).
	got := diagnose(401, `{"reason_code":"teapot"}`)
	if !strings.Contains(got, "teapot") || !strings.Contains(got, "401") {
		t.Errorf("unknown-reason diagnostic dropped raw code/status: %q", got)
	}
}

// TestDiagnose_StockCoreStatusMapping covers the reality today: core emits an
// integer `code` + a generic `message` with no machine reason code, so diagnose
// maps on status + message.
func TestDiagnose_StockCoreStatusMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"401 identity", 401, `{"code":401,"message":"invalid token or agent identity"}`, "identity rejected"},
		{"401 missing auth", 401, `{"code":401,"message":"missing authorization token"}`, "no Authorization bearer"},
		{"400 event_type", 400, `{"code":400,"message":"invalid event_type: ToolCall"}`, "accept-listed the dev event types"},
		{"400 event_type echoes value", 400, `{"code":400,"message":"invalid event_type: ToolCall"}`, "ToolCall"},
		{"400 handoff", 400, `{"code":400,"message":"handoff payload invalid"}`, "payload rejected"},
		{"400 empty msg", 400, `{"code":400}`, "no message"},
		{"500", 500, `{"code":500,"message":"internal server error"}`, "verifier/replay-cache unavailable"},
		{"non-JSON body", 418, `boom`, "418"},
		{"empty body", 502, ``, "502"},
		{"other status w/ message", 429, `{"code":429,"message":"slow down"}`, "slow down"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := diagnose(tc.status, tc.body)
			if !strings.Contains(got, tc.want) {
				t.Errorf("diagnose(%d, %q) = %q, want substring %q", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// TestDiagnose_BoundedMessage confirms a hostile/huge core message can't blow up
// a log line (reuses the upstream 1 MiB read cap + this per-line trim).
func TestDiagnose_BoundedMessage(t *testing.T) {
	huge := strings.Repeat("A", 10000)
	got := diagnose(400, `{"code":400,"message":"`+huge+`"}`)
	if len(got) > maxDiagMsg+128 { // guidance prefix/suffix + truncated msg
		t.Errorf("diagnostic not bounded: len=%d", len(got))
	}
	if !strings.Contains(got, "…") {
		t.Errorf("expected truncation marker in bounded diagnostic: %q", got)
	}
}

// TestExtractReason_IgnoresIntCodeAndNonObject verifies the forward-compat probe
// mirrors config.py: only a STRING value counts, and a non-object degrades to "".
func TestExtractReason_IgnoresIntCode(t *testing.T) {
	if r := extractReason([]byte(`{"code":401,"message":"x"}`)); r != "" {
		t.Errorf("integer code must not be read as a reason, got %q", r)
	}
	if r := extractReason([]byte(`["not","an","object"]`)); r != "" {
		t.Errorf("non-object body must yield empty reason, got %q", r)
	}
	if r := extractReason([]byte(`not json`)); r != "" {
		t.Errorf("non-JSON body must yield empty reason, got %q", r)
	}
	if r := extractReason([]byte(`{"reason_code":"signature_invalid","code":"other"}`)); r != "signature_invalid" {
		t.Errorf("reason_code must win over code, got %q", r)
	}
}

// fixedRespServer returns a server that answers every request with a fixed
// status + body, ignoring the (validly signed) request — so a test can drive the
// real Emit→post→attempt→httpError→describeDrop path against any core response.
func fixedRespServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestEmit_MapsRejection_FailOpenAndLogSafe drives the full client path: a 401
// core rejection must (a) still fail-open (VerdictUnknown, nil), (b) produce
// exactly one drop line carrying the mapped guidance + event id, and (c) never
// leak the obx_ key or Ed25519 seed (INV-1).
func TestEmit_MapsRejection_FailOpenAndLogSafe(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"stock 401 identity", 401, `{"code":401,"message":"invalid token or agent identity"}`, "identity rejected"},
		{"400 event_type (pre-EXT-core)", 400, `{"code":400,"message":"invalid event_type: ToolCall"}`, "accept-listed the dev event types"},
		{"forward-compat reason", 401, `{"reason_code":"verifier_not_configured"}`, "no KMS verifier"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fixedRespServer(t, tc.status, tc.body)
			c, log := newTestClient(t, srv.URL, false)

			v, err := c.Emit(context.Background(), sampleEvent())
			if err != nil {
				t.Fatalf("fail-open violated: Emit returned error %v", err)
			}
			if v.Verdict != VerdictUnknown {
				t.Errorf("verdict = %q, want unknown on drop", v.Verdict)
			}

			log.mu.Lock()
			nLines := len(log.lines)
			log.mu.Unlock()
			if nLines != 1 {
				t.Errorf("want exactly one diagnostic line (no retry spam), got %d: %q", nLines, log.all())
			}

			all := log.all()
			if !strings.Contains(all, tc.want) {
				t.Errorf("mapped guidance %q absent from log: %q", tc.want, all)
			}
			if !strings.Contains(all, "evt-1") {
				t.Errorf("diagnostic missing event id: %q", all)
			}
			// INV-1: no secret material in the diagnostic, ever.
			if strings.Contains(all, testAPIKey) || strings.Contains(all, testSeedB64) {
				t.Error("INV-1 violation: secret material leaked into the diagnostic")
			}
		})
	}
}
