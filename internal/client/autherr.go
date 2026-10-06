package client

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/client/workloadauth"
)

// bootstrapGuidance categories only, never content, never secrets
// (INV-1/INV-2).
var bootstrapGuidance = map[string]string{
	"invalid_api_key":               "the obx_ API key is missing, revoked or malformed; re-run `openbox init`",
	"agent_inactive":                "the workload agent was deactivated by an operator; re-run `openbox init` to register a new one",
	"workload_identity_unavailable": "the org's identity-provider generation is not initialized; ask your OpenBox admin",
	"verifier_unavailable":          "core's token verifier is unavailable (transient); retried and still failed; safe to ignore unless persistent",
}

// flat401Guidance is what an evaluate/approval/validate 401 maps to: core
// carries no reason_code on that route -- a rejected identity and any
// datastore fault while looking the token up are indistinguishable on the
// wire -- unlike a bootstrap failure.
const flat401Guidance = "API key revoked, agent deactivated or rotated, or host clock skew; run `openbox doctor`"

type coreError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const maxDiagMsg = 200

// diagnose returns a single actionable diagnostic string for a non-2xx
// /evaluate, /approval or /auth/validate response, given the HTTP status and
// the (bounded, 1 MiB-capped) response body.
func diagnose(status int, body string) string {
	raw := []byte(body)

	if reason := extractReason(raw); reason != "" {
		if g, ok := bootstrapGuidance[reason]; ok {
			return "reason=" + reason + ": " + g
		}
		return "reason=" + reason + " (status " + strconv.Itoa(status) +
			"): unrecognized reason code; re-confirm the core error envelope"
	}

	if status == 401 {
		return "401 " + flat401Guidance
	}

	var ce coreError
	_ = json.Unmarshal(raw, &ce) // best-effort; empty/non-JSON → zero value
	msg := strings.TrimSpace(ce.Message)

	switch status {
	case 400:
		if strings.HasPrefix(msg, "invalid event_type") {
			return "400 " + truncate(msg, maxDiagMsg) +
				"; core has not accept-listed the dev event types yet; events fail-open drop until it does"
		}
		if msg != "" {
			return "400 payload rejected: " + truncate(msg, maxDiagMsg)
		}
		return "400 payload rejected (no message)"
	case 500, 502, 503, 504:
		hint := "core-side fault (transient); retried and still failed; safe to ignore unless persistent"
		if msg != "" {
			return strconv.Itoa(status) + " " + hint + ": " + truncate(msg, maxDiagMsg)
		}
		return strconv.Itoa(status) + " " + hint
	default:
		if msg != "" {
			return "status " + strconv.Itoa(status) + ": " + truncate(msg, maxDiagMsg)
		}
		return "status " + strconv.Itoa(status) + " (no message)"
	}
}

// extractReason finds a machine-readable reason code, and returns "" for every
// shape core currently emits on a runtime route -- which is the correct
// answer, not a dead branch.
//
// Worth stating plainly, because it reads like a defect. Core's error
// envelope is `{Code int, Message string}`, so `code` arrives as a JSON NUMBER; each candidate below is unmarshalled into a
// `string`, so a numeric `code` fails to bind and is skipped, and the caller falls
// through to its status-plus-message diagnosis, which is correct. The `code` key still earns its place: a STRING `code` binds and
// is pinned by autherr_test.go's "string code key" case, while the numeric case
// is pinned by "401 identity". `reason_code` and `reason` are forward-compatible
// with the bootstrap route's codes and are labelled as such in the test names --
// the workload-identity bootstrap route (`/api/v3/auth/bootstrap`) DOES emit a
// string `reason_code` on failure, unlike a runtime evaluate/approval/validate
// 401, which is flat.
//
// So: do not delete this, and do not "fix" the key list to match core's numeric
// field. Both directions are already tested, and the fall-through is the design.
func extractReason(body []byte) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	for _, k := range []string{"reason_code", "code", "reason"} {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return s
		}
	}
	return ""
}

// describeWorkloadError renders a *workloadauth.Error's stage, status,
// reason and guidance -- and NEVER the token, the assertion or the key
// (INV-1); workloadauth.Error never carries any of those in the first place.
// A negative-cache Error carries Status 0 (workloadauth's Authenticator
// reconstructs it from a persisted reason with no status), so guidance keys
// off Stage/Reason, never Status.
func describeWorkloadError(e *workloadauth.Error) string {
	s := strings.TrimPrefix(e.Error(), "workloadauth: ")
	if e.Stage == workloadauth.StageBootstrap {
		if g, ok := bootstrapGuidance[e.Reason]; ok {
			s += "; " + g
		}
	}
	return s
}

// describeDrop the result never contains our key/token/signature (INV-1): the
// secret lives only in the Authorization/workload-token headers, never in the
// request or response body. A workload token never reaches here either:
// workloadauth.Error never carries it, and describeWorkloadError renders only
// stage/status/reason/guidance.
func describeDrop(err error) string {
	var werr *workloadauth.Error
	if errors.As(err, &werr) {
		return describeWorkloadError(werr)
	}
	var he *httpError
	if errors.As(err, &he) {
		return he.Error() + "; " + diagnose(he.status, he.body)
	}
	return err.Error()
}
