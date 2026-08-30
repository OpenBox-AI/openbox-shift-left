package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
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

// refusalStatus / refusalErrorType are the PROVISIONAL defaults (probe A).
//
// 403 rather than 429/500/503 because every one of those is a transience signal
// the client is built to retry. The error type is deliberately none of the
// provider's own literals (`overloaded_error`, `rate_limit_error`, `api_error`,
// `authentication_error`), so no wording-based retry rule can match it by
// accident.
const (
	refusalStatus    = http.StatusForbidden
	refusalErrorType = "openbox_policy_refusal"
)

// RefusalShape is the status and error type a refusal renders with.
//
// It is INJECTABLE for one reason: probe A has to try several candidates against a
// real Claude Code session, and a recompile per candidate makes an already
// human-gated probe more expensive than it needs to be. With this, the probe drives
// the REAL refusal path — the code that will actually ship — instead of a
// throwaway stand-in that only approximates it.
//
// This is not a config knob for orgs. It is deliberately absent from posture and
// from anything an org writes: once probe A names a shape, the defaults change and
// this stays a probe affordance.
type RefusalShape struct {
	Status    int
	ErrorType string
}

// DefaultRefusalShape is the provisional pair.
func DefaultRefusalShape() RefusalShape {
	return RefusalShape{Status: refusalStatus, ErrorType: refusalErrorType}
}

// Validate rejects a candidate that cannot possibly be right, so a probe run
// cannot waste a session on a shape the requirement already rules out.
func (r RefusalShape) Validate() error {
	if r.Status < 400 || r.Status > 499 {
		return fmt.Errorf("gateway: refusal status %d is not a 4xx — a 5xx or a 2xx tells the client something other than \"refused\"", r.Status)
	}
	for _, transient := range []int{
		http.StatusRequestTimeout, http.StatusTooManyRequests,
	} {
		if r.Status == transient {
			return fmt.Errorf("gateway: refusal status %d is a transience signal the client retries around; a policy denial would be retried, not honoured", r.Status)
		}
	}
	if r.ErrorType == "" {
		return fmt.Errorf("gateway: refusal error type is empty; the client would see an unnamed error")
	}
	for _, providerType := range []string{
		"overloaded_error", "rate_limit_error", "api_error", "authentication_error",
		"invalid_request_error", "permission_error", "not_found_error", "request_too_large",
	} {
		if r.ErrorType == providerType {
			return fmt.Errorf("gateway: refusal error type %q is the provider's own literal — a wording-based retry rule could match it", r.ErrorType)
		}
	}
	return nil
}

// Reason strings. Content-free by construction: a refusal is shown to the
// developer and stored, and neither may carry the prompt that triggered it.
const (
	reasonUnreachable = "OpenBox governance: this model call was refused because no governance " +
		"decision could be obtained — the control plane was unreachable. This is an OUTAGE, " +
		"not a policy denial, and it is refused deliberately: the gateway has no offline grace."

	// reasonCallerGone is NOT an outage, and separating the two is the whole
	// point: a developer pressing Esc cancels the request context, so every
	// interrupted turn used to produce a stored record blaming the control plane.
	// Decision.Unreachable exists so an operator can tell a denial from an outage,
	// and this is the third case it must not be confused with.
	reasonCallerGone = "OpenBox governance: this model call was not completed because the caller " +
		"went away before a governance decision arrived. Nothing was forwarded, and this is " +
		"neither a policy denial nor a control-plane outage."
)

// reasonGuardrailFailed names a guardrail block without quoting the content that
// tripped it.
//
// Only GuardrailReason.Type — the CATEGORY — is rendered. `Reason` is free text
// about the matched content and a guardrail fires on content by definition, so
// including it would put the prompt in a refusal that is both shown to the
// developer and stored (INV-2). An empty category renders "?" rather than being
// dropped, matching hookflow.ReasonTypeCategories: two renderings of one signal
// should not disagree about how many findings there were.
func reasonGuardrailFailed(e client.Evaluation) string {
	r := "OpenBox governance: this model call was refused because a content guardrail did not pass."
	if g := e.Guardrail; g != nil && len(g.Reasons) > 0 {
		cats := make([]string, 0, len(g.Reasons))
		for _, reason := range g.Reasons {
			if reason.Type == "" {
				cats = append(cats, "?")
				continue
			}
			cats = append(cats, reason.Type)
		}
		r += " Categories: [" + strings.Join(cats, ",") + "]."
	}
	return r
}

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
	WriteRefusalAs(w, d, DefaultRefusalShape())
}

// WriteRefusalAs renders with an explicit shape, so probe A can drive the real
// path with a candidate.
func WriteRefusalAs(w http.ResponseWriter, d Decision, shape RefusalShape) {
	body, err := json.Marshal(refusalBody{
		Type: "error",
		Error: refusalBodyInfo{
			Type:    shape.ErrorType,
			Message: d.Reason,
		},
	})
	if err != nil {
		// Never leave a refusal unrendered: an unwritten refusal is a forward.
		body = []byte(`{"type":"error","error":{"type":"` + shape.ErrorType +
			`","message":"OpenBox governance: refused."}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(shape.Status)
	w.Write(body)
}

// RefuseEverything is a handler that refuses every request with the given shape.
//
// PROBE A ONLY. It consults no policy and forwards nothing — it exists so a human
// running probe A can measure Claude Code's reaction to a candidate shape against
// the code that will actually ship, rather than against a throwaway server that
// merely resembles it.
//
// The reason string says PROBE explicitly. A refusal that read like a real policy
// denial would be indistinguishable from one in whatever the probe records, and the
// whole point of the probe is to know which behaviour came from where.
func RefuseEverything(shape RefusalShape) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteRefusalAs(w, Decision{
			Forward:   false,
			Evaluated: false,
			Reason: "OpenBox governance: PROBE MODE — every model call is being refused to " +
				"measure how this client reacts to the refusal shape. No policy was consulted.",
		}, shape)
	})
}
