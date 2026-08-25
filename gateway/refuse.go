package gateway

import (
	"encoding/json"
	"net/http"
)

// refuse.go renders a refusal to Claude Code.
//
// ── THE SHAPE IS PROVISIONAL AND PROBE A OWNS IT ──────────────────────────────
//
// What is decided: everything about the refusal EXCEPT the status code and the
// error `type` string. Those two come from probe A, which has not run.
//
// What the shape has to satisfy is knowable without the probe, and is what the
// tests below encode: Claude Code's retry logic matches on upstream ERROR
// WORDING, so a refusal that reads like a transient provider failure gets retried
// around, and one that reads like a capability rejection can disable a capability
// for the rest of the session — strictly worse than not refusing at all, because
// the developer then has a silently degraded session and no denial to look at.
//
// So the provisional choice is the one that is maximally UNLIKE a provider error:
// a 403 (which the provider uses for auth/permission, not for load or transience)
// carrying an error type that is unmistakably ours. If probe A finds this trips a
// retry, `refusalStatus` and `refusalErrorType` are the only two things that
// change — and if NO shape qualifies, the plan's descope applies: phase 06 becomes
// observe-only and prevention stays in the hooks.
//
// Do not treat these two constants as settled. `TestRefusalShapeIsProbePending`
// exists so a reader cannot mistake them for a verified answer.

// refusalStatus is PROVISIONAL (probe A). 403 rather than 429/500/503 because
// every one of those is a transience signal the client is built to retry.
const refusalStatus = http.StatusForbidden

// refusalErrorType is PROVISIONAL (probe A). Deliberately not any of the
// provider's own error type literals (`overloaded_error`, `rate_limit_error`,
// `api_error`, `authentication_error`), so no wording-based retry rule can match
// it by accident.
const refusalErrorType = "openbox_policy_refusal"

// Reason strings. Content-free by construction: a refusal is shown to the
// developer and stored, and neither may carry the prompt that triggered it.
const (
	reasonUnreachable = "OpenBox governance: this model call was refused because no governance " +
		"decision could be obtained — the control plane was unreachable. This is an OUTAGE, " +
		"not a policy denial, and it is refused deliberately: the gateway has no offline grace."
)

func reasonPolicyRefused(serverReason string) string {
	r := "OpenBox governance: this model call was refused by policy."
	if serverReason != "" {
		r += " " + serverReason
	}
	return r
}

func reasonApprovalRequired(ref string) string {
	r := "OpenBox governance: this model call needs approval before it can run."
	if ref != "" {
		r += " Approval reference: " + ref + "."
	}
	r += " The gateway does not hold model calls open, so the call was refused rather than queued."
	return r
}

func reasonUninterpretable(verdict string) string {
	r := "OpenBox governance: this model call was refused because the governance verdict " +
		"could not be interpreted by this build."
	if verdict != "" {
		r += " Verdict: " + verdict + "."
	}
	return r
}

// refusalBody is the JSON a refusal returns. It mirrors the provider's error
// envelope SHAPE — so a client parsing errors structurally does not crash — while
// carrying a type that is unmistakably not the provider's.
type refusalBody struct {
	Type  string          `json:"type"`
	Error refusalBodyInfo `json:"error"`
}

type refusalBodyInfo struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// WriteRefusal renders a refused decision to the caller.
//
// Requirement 6: the developer sees WHY, not a bare 4xx. The message is the whole
// point — a status code alone is indistinguishable from the gateway being broken,
// which is the failure the security note calls out.
func WriteRefusal(w http.ResponseWriter, d Decision) {
	body, err := json.Marshal(refusalBody{
		Type: "error",
		Error: refusalBodyInfo{
			Type:    refusalErrorType,
			Message: d.Reason,
		},
	})
	if err != nil {
		// Never leave a refusal unrendered: an unwritten refusal is a forward.
		body = []byte(`{"type":"error","error":{"type":"` + refusalErrorType +
			`","message":"OpenBox governance: refused."}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(refusalStatus)
	w.Write(body)
}
