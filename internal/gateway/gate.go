package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// evaluateTimeout bounds the gate's round-trip to the control plane.
//
// It exists so "the server never answered" becomes "we could not ask" instead of
// a hung model call. Chosen against the failure it converts rather than against a
// latency target: a gated call already costs a round-trip, and a developer would
// rather be refused in 10s with a reason than wait for Claude Code's own 180s
// watchdog to abort a call nobody decided on.
const evaluateTimeout = 10 * time.Second

// gate.go decides whether a model call is forwarded or refused (ADR-0021 §7).
//
// It reuses hookflow's cascade SHAPE and deliberately not its code path — the
// phase's own instruction, and correct: hookflow's ApplyFailurePolicy returns
// unchanged unless the org is fail_closed, whereas this gate refuses on a missing
// verdict UNCONDITIONALLY. Importing it would buy a function whose only branch is
// inert here, while widening this module's import surface for nothing.
//
// The posture divergence is the owner's decision (validated 2026-08-25): the
// gateway is the stronger enforcement point, so a gated model call is refused when
// /evaluate is unreachable regardless of `fail_closed`, and there is no offline
// grace — a grace window is a local verdict cache under another name.
//
// THE ORDERING RULE IS THE MERGE BLOCKER. No refusal may be synthesized before an
// evaluation has been ATTEMPTED. Pre-ADR-0017 this exact mistake shipped on the
// hook path: ApplyFailurePolicy ran before the evaluation, which was harmless only
// while the local step still produced verdicts, and the moment it stopped a
// fail-closed org denied every gated call WITHOUT EVER ASKING. Here the same
// mistake is worse, because refusal is unconditional: every blip in the control
// plane would become a total model-call outage that reports itself as a policy
// decision no policy made. Decision.Evaluated exists so that invariant is
// checkable rather than reviewable, and TestNoRefusalWithoutAnEvaluationAttempt is
// its control.

// Evaluator is the seam the gate calls to reach /evaluate. An interface so the
// ordering control can drive it without a network, and so the gateway never grows
// its own transport.
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

	// Evaluated records whether an evaluation was ATTEMPTED. It is the ordering
	// invariant made checkable: a refusal with Evaluated false means a synthesized
	// refusal fired before anything asked the control plane, which is the bug this
	// phase exists to prevent from recurring.
	Evaluated bool

	// Unreachable distinguishes "policy refused you" from "we could not ask".
	// Without it a core outage is indistinguishable from a denial, and the
	// developer debugs the wrong thing.
	Unreachable bool
}

// Decide runs the gate for one captured model call.
//
// gated comes from policy, never from this engine second-guessing it. ADR-0017's
// lesson applies verbatim: the narrowing that let the engine decide which calls
// deserved a real verdict is why a raw-rego org was ungoverned on everything but
// shell and MCP. Risk is a property of the POLICY.
func Decide(ctx context.Context, ev Evaluator, gated bool, c Captured) Decision {
	// Ungated: no round-trip at all. Requirement 5, and the reason per-call
	// gating does not tank latency — roughly 52 model calls per turn window were
	// measured, so a round-trip per call is not affordable.
	if !gated {
		return Decision{Forward: true, Evaluated: false}
	}

	// ATTEMPT FIRST. Everything below this line depends on having asked.
	//
	// With a DEADLINE of its own. The caller's context is the request's, and the
	// gateway's client deliberately has no overall timeout (a streamed completion
	// runs for minutes) — so a control plane that accepts the connection and never
	// answers left the gated call hanging indefinitely: neither forwarded nor
	// refused, which is the one outcome the always-refuse posture claims to have
	// eliminated. "Never answers" is not "unreachable", and only a deadline turns
	// the first into the second.
	evalCtx, cancel := context.WithTimeout(ctx, evaluateTimeout)
	defer cancel()
	evaluation, err := ev.Evaluate(evalCtx, c)

	if err != nil {
		// THE CALLER GOING AWAY IS NOT AN OUTAGE. r.Context() is cancelled the
		// moment the developer interrupts a turn, so every Esc produced a durable
		// record saying the control plane was unreachable — and Unreachable exists
		// precisely so an operator can tell an outage from a denial, so it was the
		// field being filled in with the wrong answer. Nothing is forwarded either
		// way; what changes is what the record claims happened.
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return Decision{
				Forward:   false,
				Evaluated: true,
				Reason:    reasonCallerGone,
			}
		}
		// Always refuse — regardless of fail_closed, with no grace window.
		return Decision{
			Forward:     false,
			Evaluated:   true,
			Unreachable: true,
			Reason:      reasonUnreachable,
		}
	}

	// A FAILED GUARDRAIL IS A BLOCK, and it is checked here for the same reason
	// hookflow.MapVerdict checks it in this exact position (enforce.go:484-486):
	// after HALT/BLOCK, before REQUIRE_APPROVAL. Reading only `Verdict` meant one
	// /evaluate response — ALLOW with guardrails_result.passed:false — denied the
	// tool call on the hook path and FORWARDED the model call here, credential and
	// prompt included, on the surface ADR-0021 §7 calls the stronger enforcement
	// point. The reference SDK's enforce_verdict treats it as a block too
	// (client/verdict.go:132-134 records that), so this was an omission rather
	// than the deliberate divergence the header comment enumerates.
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
		// CONSTRAIN forwards. It is non-blocking everywhere else in this repo —
		// hookflow's cascade treats CONSTRAIN, ALLOW and UNKNOWN alike — and having
		// the gateway alone refuse it would mean one policy verdict silently
		// meaning "proceed" for tool calls and "deny" for model calls. That
		// divergence is not the owner's always-refuse decision, which is about a
		// MISSING verdict, not about a verdict that says go ahead.
		return Decision{Forward: true, Evaluated: true, Verdict: evaluation.Verdict}

	case client.VerdictRequireApproval:
		// No approval hold in v1: holding a model call open depends on the ping
		// mechanism probe A2 has not proven, and the event-level watchdog wants
		// PARSED events rather than bytes. Refusing with the reference is the
		// same call Codex made for `ask` (OD-SL7-ASK) and for the same reason —
		// a fallthrough that proceeds ungoverned is worse than an over-ask.
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
		// An empty or unrecognized verdict is NOT an allow.
		//
		// Note the asymmetry with CONSTRAIN above, which is deliberate: a verdict
		// this build KNOWS and knows to be non-blocking forwards; a verdict it
		// cannot interpret refuses. Folding the two together in either direction
		// would either block a legitimate CONSTRAIN or wave through a future
		// blocking verdict this build has never heard of. Rendering nothing for
		// an unknown literal is how Codex would have made HALT silently proceed
		// (ADR-0020); the same trap is avoided here by defaulting to refusal on a
		// verdict this build cannot interpret.
		return Decision{
			Forward:   false,
			Evaluated: true,
			Verdict:   evaluation.Verdict,
			Reason:    reasonUninterpretable(string(evaluation.Verdict)),
		}
	}
}
