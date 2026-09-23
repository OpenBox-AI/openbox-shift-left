package client

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"strings"
	"testing"
)

// TestDiagnose_ForwardCompatReasonCodes feeds each bootstrap reason code as a
// string body field (the shape the workload-identity bootstrap route emits on
// failure) and asserts the mapped, actionable guidance.
func TestDiagnose_ForwardCompatReasonCodes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string // substring the guidance must contain
	}{
		{"invalid_api_key", 401, `{"reason_code":"invalid_api_key"}`, "revoked or malformed"},
		{"agent_inactive", 403, `{"reason_code":"agent_inactive"}`, "deactivated by an operator"},
		{"workload_identity_unavailable", 409, `{"reason_code":"workload_identity_unavailable"}`, "identity-provider generation"},
		{"verifier_unavailable", 503, `{"reason_code":"verifier_unavailable"}`, "token verifier is unavailable"},
		{"legacy reason key", 403, `{"reason":"agent_inactive"}`, "deactivated by an operator"},
		{"string code key", 403, `{"code":"agent_inactive"}`, "deactivated by an operator"},
		{"unknown reason", 400, `{"reason_code":"teapot"}`, "unrecognized reason code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := diagnose(tc.status, tc.body)
			if !strings.Contains(got, tc.want) {
				t.Errorf("diagnose(%d, %s) = %q, want substring %q", tc.status, tc.body, got, tc.want)
			}
		})
	}
	got := diagnose(400, `{"reason_code":"teapot"}`)
	if !strings.Contains(got, "teapot") || !strings.Contains(got, "400") {
		t.Errorf("unknown-reason diagnostic dropped raw code/status: %q", got)
	}
}

// TestDiagnose_StockCoreStatusMapping covers the reality today: core emits an
// integer `code` + a generic `message` with no machine reason code on a
// runtime route (evaluate/approval/validate), so diagnose maps on status +
// message -- and a runtime 401 is flat: the same body a rejected identity
// and a datastore fault both produce.
func TestDiagnose_StockCoreStatusMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"401 identity", 401, `{"code":401,"message":"invalid token or agent identity"}`, "run `openbox doctor`"},
		{"401 no message", 401, `{"code":401}`, "run `openbox doctor`"},
		{"400 event_type", 400, `{"code":400,"message":"invalid event_type: ToolCall"}`, "accept-listed the dev event types"},
		{"400 event_type echoes value", 400, `{"code":400,"message":"invalid event_type: ToolCall"}`, "ToolCall"},
		{"400 handoff", 400, `{"code":400,"message":"handoff payload invalid"}`, "payload rejected"},
		{"400 empty msg", 400, `{"code":400}`, "no message"},
		{"500", 500, `{"code":500,"message":"internal server error"}`, "internal server error"},
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

// TestDiagnose_BoundedMessage confirms a hostile/huge core message can't blow
// up a log line (reuses the upstream 1 MiB read cap + this per-line trim).
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

// TestExtractReason_IgnoresIntCodeAndNonObject verifies the forward-compat
// probe mirrors config.py: only a string value counts, and a non-object
// degrades to "".
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
	if r := extractReason([]byte(`{"reason_code":"agent_inactive","code":"other"}`)); r != "agent_inactive" {
		t.Errorf("reason_code must win over code, got %q", r)
	}
}

func fixedRespServer(t *testing.T, status int, body string) *memhttptest.Server {
	t.Helper()
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestEmit_MapsRejection_FailOpenAndLogSafe drives the full client path: a
// rejection must (a) still fail-open on the verdict (VerdictUnknown, which no
// caller reads as a block) while reporting the advisory ErrDelivery so a
// durable caller can retry (E8-S7), (b) produce exactly one log line carrying
// the mapped guidance + event id, and (c) never leak the obx_ key or workload
// bearer (INV-1).
func TestEmit_MapsRejection_FailOpenAndLogSafe(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"stock 401, flat", 401, `{"code":401,"message":"invalid token or agent identity"}`, "run `openbox doctor`"},
		{"400 event_type (pre-EXT-core)", 400, `{"code":400,"message":"invalid event_type: ToolCall"}`, "accept-listed the dev event types"},
		{"forward-compat bootstrap reason", 403, `{"reason_code":"verifier_unavailable"}`, "token verifier is unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fixedRespServer(t, tc.status, tc.body)
			c, log := newTestClient(t, srv.URL, false)

			v, err := c.Emit(context.Background(), sampleEvent())
			if !errors.Is(err, ErrDelivery) {
				t.Fatalf("want ErrDelivery so the spool can retry, got %v", err)
			}
			if v.Verdict != VerdictUnknown {
				t.Errorf("fail-open violated: verdict = %q, want unknown on drop", v.Verdict)
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
			if strings.Contains(all, testAPIKey) || strings.Contains(all, testWorkloadToken) {
				t.Error("INV-1 violation: secret material leaked into the diagnostic")
			}
		})
	}
}
