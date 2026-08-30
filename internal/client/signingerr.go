package client

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// signingReasonGuidance maps a machine reason code; as enumerated by openbox-
// core's AIP identity verifier and the reference SDK's map_signing_error; to a
// shift-left-actionable one-line diagnostic.
var signingReasonGuidance = map[string]string{
	"signature_invalid":        "the signed bytes were rejected — a rotated/mismatched Ed25519 key or a body-hash mismatch; re-provision the dev agent (docs/getting-started.md § Troubleshooting)",
	"nonce_replayed":           "a buffered event was re-sent after a lost 200 (INV-5); safe to ignore unless persistent",
	"did_agent_mismatch":       "the agent DID does not match the key the obx_ credential was provisioned for — re-run `openbox init` for this provider (INV-7)",
	"verifier_not_configured":  "the dev agent has no KMS verifier; register signing-off or set signing_required=false (docs/getting-started.md § Troubleshooting)",
	"timestamp_outside_window": "the request timestamp is outside core's ±300s window — sync the host clock (NTP)",
	"timestamp_skew":           "the request timestamp is outside core's ±300s window — sync the host clock (NTP)",
}

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
			"): unrecognized reason code — re-confirm the core error envelope"
	}

	var ce coreError
	_ = json.Unmarshal(raw, &ce) // best-effort; empty/non-JSON → zero value
	msg := strings.TrimSpace(ce.Message)

	switch status {
	case 401:
		if strings.Contains(msg, "missing authorization") {
			return "401 no Authorization bearer — the obx_ credential is missing/empty; re-run `openbox init`"
		}
		return "401 identity rejected — core does not disclose the specific reason over HTTP; " +
			"likely no KMS verifier (set signing_required=false), a rotated/mismatched key, or host clock skew (docs/getting-started.md § Troubleshooting)"
	case 400:
		if strings.HasPrefix(msg, "invalid event_type") {
			return "400 " + truncate(msg, maxDiagMsg) +
				" — core has not accept-listed the dev event types yet; events fail-open drop until it does (INV-8)"
		}
		if msg != "" {
			return "400 payload rejected: " + truncate(msg, maxDiagMsg)
		}
		return "400 payload rejected (no message)"
	case 500:
		if strings.Contains(msg, "verifier") {
			return "500 " + signingReasonGuidance["verifier_not_configured"]
		}
		return "500 core-side verifier/replay-cache unavailable (transient); retried and still failed — safe to ignore unless persistent"
	default:
		if msg != "" {
			return "status " + strconv.Itoa(status) + ": " + truncate(msg, maxDiagMsg)
		}
		return "status " + strconv.Itoa(status) + " (no message)"
	}
}

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

// describeDrop turns a fail-open drop error into a single actionable
// diagnostic line for Emit's drop log.
func describeDrop(err error) string {
	var he *httpError
	if errors.As(err, &he) {
		return he.Error() + " — " + diagnose(he.status, he.body)
	}
	return err.Error()
}
