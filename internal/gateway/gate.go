package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// evaluateTimeout bounds the gate's round-trip to the control plane. It exists
// so "the server never answered" becomes "we could not ask" instead of a hung
// model call.
const evaluateTimeout = 10 * time.Second

// Evaluator is the seam the gate calls to reach /evaluate. An interface so the
// ordering control can drive it without a network, and so the gateway never
// grows its own transport.
type Evaluator interface {
	Evaluate(ctx context.Context, c Captured) (client.Evaluation, error)
}

// Decision is the gate's answer for one model call.
type Decision struct {
	// Forward is true when the call proceeds untouched.
	Forward bool

	// Verdict is the server's answer, or empty when none was obtained.
	Verdict client.Verdict

	// Reason is what the developer is shown. Never a bare status.
	Reason string

	// Evaluated records whether an evaluation was attempted.
	Evaluated bool

	// Unreachable distinguishes "policy refused you" from "we could not ask".
	Unreachable bool
}

// Decide runs the gate for one captured model call. Gated comes from policy,
// never from this engine second-guessing it.
func Decide(ctx context.Context, ev Evaluator, gated bool, c Captured) Decision {
	if !gated {
		return Decision{Forward: true, Evaluated: false}
	}

	evalCtx, cancel := context.WithTimeout(ctx, evaluateTimeout)
	defer cancel()
	evaluation, err := ev.Evaluate(evalCtx, c)

	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return Decision{
				Forward:   false,
				Evaluated: true,
				Reason:    reasonCallerGone,
			}
		}
		return Decision{
			Forward:     false,
			Evaluated:   true,
			Unreachable: true,
			Reason:      reasonUnreachable,
		}
	}

	if g := evaluation.Guardrail; g != nil && !g.Passed {
		return Decision{
			Forward:   false,
			Evaluated: true,
			Verdict:   evaluation.Verdict,
			Reason:    reasonGuardrailFailed(evaluation),
		}
	}

	switch evaluation.Verdict {
	case client.VerdictAllow, client.VerdictConstrain:
		return Decision{Forward: true, Evaluated: true, Verdict: evaluation.Verdict}

	case client.VerdictRequireApproval:
		return Decision{
			Forward:   false,
			Evaluated: true,
			Verdict:   evaluation.Verdict,
			Reason:    reasonApprovalRequired(evaluation.ApprovalRef()),
		}

	case client.VerdictHalt, client.VerdictBlock:
		return Decision{
			Forward:   false,
			Evaluated: true,
			Verdict:   evaluation.Verdict,
			Reason:    reasonPolicyRefused(evaluation.Reason),
		}

	default:
		return Decision{
			Forward:   false,
			Evaluated: true,
			Verdict:   evaluation.Verdict,
			Reason:    reasonUninterpretable(string(evaluation.Verdict)),
		}
	}
}
