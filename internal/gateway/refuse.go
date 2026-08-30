package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

const (
	refusalStatus    = http.StatusForbidden
	refusalErrorType = "openbox_policy_refusal"
)

// RefusalShape is the status and error type a refusal renders with. This is
// not a config knob for orgs. It is deliberately absent from posture and from
// anything an org writes: once probe A names a shape, the defaults change and
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

const (
	reasonUnreachable = "OpenBox governance: this model call was refused because no governance " +
		"decision could be obtained — the control plane was unreachable. This is an OUTAGE, " +
		"not a policy denial, and it is refused deliberately: the gateway has no offline grace."

	// reasonCallerGone decision.Unreachable exists so an operator can tell a
	// denial from an outage, and this is the third case it must not be confused
	// with.
	reasonCallerGone = "OpenBox governance: this model call was not completed because the caller " +
		"went away before a governance decision arrived. Nothing was forwarded, and this is " +
		"neither a policy denial nor a control-plane outage."
)

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

type refusalBody struct {
	Type  string          `json:"type"`
	Error refusalBodyInfo `json:"error"`
}

type refusalBodyInfo struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// WriteRefusal renders a refused decision to the caller.
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
		body = []byte(`{"type":"error","error":{"type":"` + shape.ErrorType +
			`","message":"OpenBox governance: refused."}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(shape.Status)
	w.Write(body)
}

// RefuseEverything is a handler that refuses every request with the given
// shape.
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
