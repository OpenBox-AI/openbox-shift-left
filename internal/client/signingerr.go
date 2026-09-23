package client

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/client/workloadauth"
)

// signingReasonGuidance categories only, never content, never secrets
// (INV-1/INV-2).
var signingReasonGuidance = map[string]string{
	"signature_invalid":        "the signed bytes were rejected; a rotated/mismatched Ed25519 key or a body-hash mismatch; re-provision the dev agent (docs/getting-started.md § Troubleshooting)",
	"nonce_replayed":           "a buffered event was re-sent after a lost 200 (INV-5); safe to ignore unless persistent",
	"did_agent_mismatch":       "the agent DID does not match the key the obx_ credential was provisioned for; re-run `openbox init` for this provider (INV-7)",
	"verifier_not_configured":  "the dev agent has no KMS verifier; register signing-off or set signing_required=false (docs/getting-started.md § Troubleshooting)",
	"timestamp_outside_window": "the request timestamp is outside core's ±300s window; sync the host clock (NTP)",
	"timestamp_skew":           "the request timestamp is outside core's ±300s window; sync the host clock (NTP)",
}

// v3BootstrapGuidance categories only, never content, never secrets
// (INV-1/INV-2). Added slice 4a, alongside signingReasonGuidance rather than
// in place of it: v1 stays byte-identical until slice 4d deletes it.
var v3BootstrapGuidance = map[string]string{
	"invalid_api_key":               "the obx_ API key is missing, revoked or malformed; re-run `openbox init`",
	"agent_inactive":                "the workload agent was deactivated by an operator; re-run `openbox init` to register a new one",
	"workload_identity_unavailable": "the org's identity-provider generation is not initialized; ask your OpenBox admin",
	"verifier_unavailable":          "core's token verifier is unavailable (transient); retried and still failed; safe to ignore unless persistent",
}

// v3Flat401Guidance is what a v3 evaluate/approval/validate 401 maps to: core
// carries no reason_code on that route (D2), unlike bootstrap.
const v3Flat401Guidance = "API key revoked, agent deactivated or rotated, or host clock skew; run `openbox doctor`"

type coreError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const maxDiagMsg = 200

// diagnose returns a single actionable diagnostic string for a non-2xx
// /evaluate response, given the HTTP status and the (bounded, 1 MiB-capped)
// response body.
func diagnose(status int, body string) string {
	raw := []byte(body)

	if reason := extractReason(raw); reason != "" {
		if g, ok := signingReasonGuidance[reason]; ok {
			return "reason=" + reason + ": " + g
		}
		return "reason=" + reason + " (status " + strconv.Itoa(status) +
			"): unrecognized reason code; re-confirm the core error envelope"
	}

	var ce coreError
	_ = json.Unmarshal(raw, &ce) // best-effort; empty/non-JSON → zero value
	msg := strings.TrimSpace(ce.Message)

	switch status {
	case 401:
		if strings.Contains(msg, "missing authorization") {
			return "401 no Authorization bearer; the obx_ credential is missing/empty; re-run `openbox init`"
		}
		return "401 identity rejected; core does not disclose the specific reason over HTTP; " +
			"likely no KMS verifier (set signing_required=false), a rotated/mismatched key, or host clock skew (docs/getting-started.md § Troubleshooting)"
	case 400:
		if strings.HasPrefix(msg, "invalid event_type") {
			return "400 " + truncate(msg, maxDiagMsg) +
				"; core has not accept-listed the dev event types yet; events fail-open drop until it does (INV-8)"
		}
		if msg != "" {
			return "400 payload rejected: " + truncate(msg, maxDiagMsg)
		}
		return "400 payload rejected (no message)"
	case 500:
		if strings.Contains(msg, "verifier") {
			return "500 " + signingReasonGuidance["verifier_not_configured"]
		}
		return "500 core-side verifier/replay-cache unavailable (transient); retried and still failed; safe to ignore unless persistent"
	default:
		if msg != "" {
			return "status " + strconv.Itoa(status) + ": " + truncate(msg, maxDiagMsg)
		}
		return "status " + strconv.Itoa(status) + " (no message)"
	}
}

// extractReason finds a machine-readable reason code, and returns "" for every
// shape core currently emits -- which is the correct answer, not a dead branch.
//
// Worth stating plainly, because it has now misread twice as a defect. Core's
// envelope is `{Code int, Message string}` (`openbox-core/pkg/httpx/response.go:12-16`),
// so `code` arrives as a JSON NUMBER; each candidate below is unmarshalled into a
// `string`, so a numeric `code` fails to bind and is skipped, and the caller falls
// through to its status-plus-message diagnosis. All 555 observed 401s took that
// path, correctly. The `code` key still earns its place: a STRING `code` binds and
// is pinned by signingerr_test.go's "string code key" case, while the numeric case
// is pinned by "401 identity". `reason_code` and `reason` are forward-compatible
// with the SDK's codes and are labelled as such in the test names.
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

// diagnoseV3 is diagnose's v3 counterpart: a bootstrap failure still carries a
// reason_code (unchanged from v1's envelope), but a runtime 401
// (evaluate/approval/validate) is flat (D2), so the same
// {"code":401,"message":"invalid token or agent identity"} body v1 renders as
// "identity rejected" here renders the v3 guidance instead. The two never
// collide on the wire: diagnoseV3 is reached only for an httpError with v3
// set true (attemptV3/validateV3), so a v1-mode failure always goes through
// diagnose, unaffected.
func diagnoseV3(status int, body string) string {
	raw := []byte(body)

	if reason := extractReason(raw); reason != "" {
		if g, ok := v3BootstrapGuidance[reason]; ok {
			return "reason=" + reason + ": " + g
		}
		return "reason=" + reason + " (status " + strconv.Itoa(status) +
			"): unrecognized reason code; re-confirm the core error envelope"
	}

	if status == 401 {
		return "401 " + v3Flat401Guidance
	}

	var ce coreError
	_ = json.Unmarshal(raw, &ce)
	msg := strings.TrimSpace(ce.Message)
	if msg != "" {
		return "status " + strconv.Itoa(status) + ": " + truncate(msg, maxDiagMsg)
	}
	return "status " + strconv.Itoa(status) + " (no message)"
}

// describeWorkloadError renders a *workloadauth.Error's stage, status,
// reason and guidance -- and NEVER the token, the assertion or the key
// (INV-1); workloadauth.Error never carries any of those in the first place.
// A negative-cache Error carries Status 0 (workloadauth's Authenticator
// reconstructs it from a persisted reason with no status), so guidance keys
// off Stage/Reason, never Status.
func describeWorkloadError(e *workloadauth.Error) string {
	var b strings.Builder
	b.WriteString(string(e.Stage))
	if e.Status != 0 {
		b.WriteString(" (status ")
		b.WriteString(strconv.Itoa(e.Status))
		b.WriteString(")")
	}
	if e.Reason != "" {
		b.WriteString(": reason=")
		b.WriteString(e.Reason)
		if e.Stage == workloadauth.StageBootstrap {
			if g, ok := v3BootstrapGuidance[e.Reason]; ok {
				b.WriteString(": ")
				b.WriteString(g)
			}
		}
	}
	if e.Detail != "" {
		b.WriteString(" (")
		b.WriteString(e.Detail)
		b.WriteString(")")
	}
	if e.Hint != "" {
		b.WriteString("; ")
		b.WriteString(e.Hint)
	}
	return b.String()
}

// describeDrop the result never contains our key/seed/nonce/signature (INV-1):
// the secret lives only in the Authorization header, never in the request or
// response body. A v3 workload token never reaches here either:
// workloadauth.Error never carries it, and describeWorkloadError renders only
// stage/status/reason/guidance.
func describeDrop(err error) string {
	var werr *workloadauth.Error
	if errors.As(err, &werr) {
		return describeWorkloadError(werr)
	}
	var he *httpError
	if errors.As(err, &he) {
		if he.v3 {
			return he.Error() + "; " + diagnoseV3(he.status, he.body)
		}
		return he.Error() + "; " + diagnose(he.status, he.body)
	}
	return err.Error()
}
