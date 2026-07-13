package client

import (
	"encoding/json"
	"errors"
	"strings"
)

// signingReasonGuidance maps a machine reason code — as enumerated by openbox
// -core's AIP identity verifier (openbox-core internal/services/agent.go) and by
// the reference SDK's map_signing_error (openbox-temporal-sdk-python/openbox/
// errors.py) — to a shift-left-actionable one-line diagnostic. Categories only,
// never content, never secrets (INV-1/INV-2).
//
// IMPORTANT (verified against openbox-core, 2026-07-13): stock core does NOT put
// these reason codes in the HTTP response body. agent.go records them as
// server-side slog fields only, and the /evaluate handler collapses every
// identity failure into 401 {"code":401,"message":"invalid token or agent
// identity"} (pkg/httpx/response.go body{code,message,data}). So this map is
// consulted via a FORWARD-COMPATIBLE body probe (extractReason) that fires only
// if a future core (EXT-core) enriches the envelope with a string reason field —
// mirroring the SDK's config.py _extract_reason_code key order. Until then
// diagnose() maps on the fields that ARE present (status + message). This keeps
// the SDK-parity map ready without guessing a schema stock core does not emit
// (STORY-SL-10 stop condition).
var signingReasonGuidance = map[string]string{
	"signature_invalid":        "the signed bytes were rejected — a rotated/mismatched Ed25519 key or a body-hash mismatch; re-provision the dev agent (RUNBOOK §3.2)",
	"nonce_replayed":           "a buffered event was re-sent after a lost 200 (INV-5); safe to ignore unless persistent",
	"did_agent_mismatch":       "the agent DID does not match the key the obx_ credential was provisioned for — re-run `openbox dev init` for this provider (INV-7)",
	"verifier_not_configured":  "the dev agent has no KMS verifier; register signing-off or set signing_required=false (RUNBOOK §3.2)",
	"timestamp_outside_window": "the request timestamp is outside core's ±300s window — sync the host clock (NTP)",
	"timestamp_skew":           "the request timestamp is outside core's ±300s window — sync the host clock (NTP)",
}

// coreError is openbox-core's uniform error envelope (pkg/httpx/response.go
// body{code,message,data}); every non-2xx /evaluate response is one of these,
// e.g. {"code":401,"message":"invalid token or agent identity"}. `code` is an
// integer echo of the HTTP status, not a machine reason code.
type coreError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// maxDiagMsg bounds how much of core's message a single diagnostic line echoes.
// The response body is already capped at 1 MiB upstream (attempt); this keeps
// the log line readable and can't be inflated by a hostile message.
const maxDiagMsg = 200

// diagnose returns a single actionable diagnostic string for a non-2xx /evaluate
// response, given the HTTP status and the (bounded, 1 MiB-capped) response body.
// It derives its output only from static guidance, core's own status/message,
// and a core-supplied reason code — never from our key/seed/nonce/signature
// (INV-1), and never from event content (INV-2).
func diagnose(status int, body string) string {
	raw := []byte(body)

	// Forward-compat: if a future core enriches the envelope with a machine
	// reason code (a STRING under reason_code|code|reason — SDK config.py order),
	// map it directly. Stock core emits an INTEGER `code`, which is skipped here.
	if reason := extractReason(raw); reason != "" {
		if g, ok := signingReasonGuidance[reason]; ok {
			return "reason=" + reason + ": " + g
		}
		return "reason=" + reason + " (status " + itoa(status) +
			"): unrecognized reason code — re-confirm the core error envelope (EXT-core)"
	}

	// Stock core: map on the fields that ARE present (status + message).
	var ce coreError
	_ = json.Unmarshal(raw, &ce) // best-effort; empty/non-JSON → zero value
	msg := strings.TrimSpace(ce.Message)

	switch status {
	case 401:
		if strings.Contains(msg, "missing authorization") {
			return "401 no Authorization bearer — the obx_ credential is missing/empty; re-run `openbox dev init`"
		}
		return "401 identity rejected — core does not disclose the specific reason over HTTP; " +
			"likely no KMS verifier (set signing_required=false), a rotated/mismatched key, or host clock skew (RUNBOOK §3.2)"
	case 400:
		if strings.HasPrefix(msg, "invalid event_type") {
			return "400 " + truncate(msg, maxDiagMsg) +
				" — core has not accept-listed the dev event types yet; events fail-open drop until EXT-core adds them (arch D4/INV-8)"
		}
		if msg != "" {
			return "400 payload rejected: " + truncate(msg, maxDiagMsg)
		}
		return "400 payload rejected (no message)"
	case 500:
		return "500 core-side verifier/replay-cache unavailable (transient); retried and still failed — safe to ignore unless persistent"
	default:
		if msg != "" {
			return "status " + itoa(status) + ": " + truncate(msg, maxDiagMsg)
		}
		return "status " + itoa(status) + " (no message)"
	}
}

// extractReason tracks the reference SDK's config.py _extract_reason_code: parse
// the body as a JSON object and return the first STRING value among reason_code,
// code, reason (in that order). A non-object body, absent keys, or a non-string
// value (e.g. stock core's integer `code`) yields "". Best-effort: any parse
// failure degrades to "" so diagnose falls back to the status/message path.
//
// One INTENTIONAL divergence from config.py's `a or b or c` short-circuit: Python
// stops at a truthy-but-non-string `code` (an int) and returns None, which would
// mask a valid string `reason` sitting behind it. This loop instead skips a
// non-string value and keeps looking — strictly better, and it never misreads
// core's integer `code` as a reason. The chosen order is still honored.
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

// describeDrop turns a fail-open drop error into a single actionable diagnostic
// line for Emit's drop log. For a core HTTP rejection (*httpError, which already
// carries the bounded response body from attempt) it appends the mapped
// signing/response guidance; a transport (network) error is returned verbatim.
// The result never contains our key/seed/nonce/signature (INV-1): the secret
// lives only in the Authorization header, never in the request or response body.
func describeDrop(err error) string {
	var he *httpError
	if errors.As(err, &he) {
		return he.Error() + " — " + diagnose(he.status, he.body)
	}
	return err.Error()
}
