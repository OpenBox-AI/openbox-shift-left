package hookflow

import (
	"context"
	"log"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

const (
	SourceEvaluate         = "evaluate"
	SourceEvaluateFailOpen = "evaluate:fail-open"
)

// DefaultEvaluationTimeout is the default budget for one evaluation.
const DefaultEvaluationTimeout = 3500 * time.Millisecond

// HookBudgetMargin is the slack reserved under the provider's declared gating
// ceiling for the non-gate work that brackets the gate: config reads,
// classify, apply, spool, audit. 1s is generous for in-process work plus one
// bounded stdout write.
const HookBudgetMargin = 1 * time.Second

// EnforceBudget is the whole-hook wall clock the gate may spend: the
// provider's declared gating ceiling less the margin.
func EnforceBudget(c provider.HookCeiling) time.Duration {
	return c.Gating - HookBudgetMargin
}

// Evaluator obtains the authoritative verdict for a gated tool call: one
// bounded, synchronous /evaluate round-trip, run before the tool does.
type Evaluator struct {
	// Ceiling is the provider's declared hook-kill limit.
	Ceiling provider.HookCeiling
	// MaxTimeout clamps the configured per-evaluation budget.
	MaxTimeout time.Duration
	// NewClient builds the control-plane transport for the evaluation and for the
	// approval hold that can follow it.
	NewClient func(*log.Logger) (Governor, error)

	// OnDelivered, when set, is called once the escalation's POST has returned
	// without a transport error; the point at which core has stored this event.
	// It is keyed on transport success, deliberately, NOT on the verdict.
	OnDelivered func()
}

// Governor is the control-plane transport the enforce path needs: escalate an
// event for an authoritative verdict, and read back where a filed approval
// stands.
type Governor interface {
	Emitter
	PollApproval(ctx context.Context, key client.ApprovalKey) (client.ApprovalStatus, error)
}

// Budget is the effective budget for one evaluation: the configured budget,
// but never more than the time remaining in the whole-hook budget after the
// local step ran. EnforceStart is the instant the enforce block began.
func (t Evaluator) Budget(enforceStart time.Time, configured time.Duration) time.Duration {
	budget := configured
	if t.MaxTimeout > 0 && budget > t.MaxTimeout {
		budget = t.MaxTimeout
	}
	if rem := t.remaining(enforceStart); rem < budget {
		budget = rem
	}
	return budget
}

// remaining every budget the gate hands out is clamped by it, so the
// sequential steps can never jointly overrun the provider's hook timeout
// however they are configured individually.
func (t Evaluator) remaining(enforceStart time.Time) time.Duration {
	return EnforceBudget(t.Ceiling) - time.Since(enforceStart)
}

// Escalate runs one bounded evaluation for an already-mapped event.
func (t Evaluator) Escalate(ctx context.Context, logger *log.Logger, ev client.DevEvent, budget time.Duration) decision.Decision {
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	resultCh := make(chan decision.Decision, 1)
	go func() { resultCh <- t.run(cctx, logger, ev) }()

	select {
	case dec := <-resultCh:
		return dec
	case <-cctx.Done():
		logger.Printf("inline evaluation degrading (budget %v exceeded)", budget)
		return EvaluationFailOpen("evaluation budget exceeded")
	}
}

func (t Evaluator) run(cctx context.Context, logger *log.Logger, ev client.DevEvent) decision.Decision {
	if t.NewClient == nil {
		logger.Print("evaluation degrading: no control-plane transport configured")
		return EvaluationFailOpen("control plane unreachable")
	}
	cl, err := t.NewClient(logger)
	if err != nil {
		logger.Printf("inline evaluation degrading (client init): %v", err)
		return EvaluationFailOpen("control plane unreachable")
	}
	eval, err := cl.Emit(cctx, ev)
	if err != nil {
		logger.Printf("inline evaluation degrading (emit): %v", err)
		return EvaluationFailOpen("evaluation undelivered")
	}
	if t.OnDelivered != nil {
		t.OnDelivered()
	}
	return EvaluationDecision(eval)
}

// EvaluationFailOpen is the degraded escalation outcome: no real verdict,
// marked fail-open so the org's failure policy decides what happens next.
func EvaluationFailOpen(cause string) decision.Decision {
	return decision.Decision{
		Evaluation: client.Evaluation{Verdict: client.VerdictUnknown, Reason: cause},
		FailOpen:   true,
		Source:     SourceEvaluateFailOpen,
	}
}

func EvaluationDecision(eval client.Evaluation) decision.Decision {
	if eval.Verdict == client.VerdictUnknown {
		return EvaluationFailOpen("/evaluate returned no verdict")
	}
	return decision.Decision{Evaluation: eval, Source: SourceEvaluate}
}

// DecisionTightens reports whether a decision would produce a deny/ask; i.e.
// Whether it is an answer that already restricts the call.
func DecisionTightens(dec decision.Decision, c OutputContract) bool {
	d, _ := MapVerdict(dec.Evaluation, c)
	return d != ""
}

func resolveEvaluationTimeout() time.Duration { return DefaultEvaluationTimeout }
