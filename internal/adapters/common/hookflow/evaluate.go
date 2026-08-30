package hookflow

import (
	"context"
	"log"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// Decision sources for the inline evaluation, distinct from the local bundle's so
// the audit can tell where a verdict came from.
const (
	SourceEvaluate         = "evaluate"
	SourceEvaluateFailOpen = "evaluate:fail-open"
)

// DefaultEvaluationTimeout is the default budget for one evaluation. It sits under
// the hook budget so a slow control plane degrades rather than trips the
// provider's hook kill.
const DefaultEvaluationTimeout = 3500 * time.Millisecond

// HookBudgetMargin is the slack reserved under the provider's declared gating
// ceiling for the non-gate work that brackets the gate: config reads, classify,
// apply, spool, audit. 1s is generous for in-process work plus one bounded
// stdout write.
//
// It lives here rather than in each adapter because it describes what the ENGINE
// does around the gate, which is the same work whatever the provider. Both
// adapters previously declared their own identical copy next to their own
// hardcoded ceiling.
const HookBudgetMargin = 1 * time.Second

// EnforceBudget is the whole-hook wall clock the gate may spend: the provider's
// declared gating ceiling less the margin. The local step, the evaluation and any
// approval hold run sequentially inside it, so their individually-clamped
// budgets can never jointly push the hook past the provider's kill.
//
// A ceiling at or below the margin yields a non-positive budget, which makes
// every evaluation fail open immediately rather than risk a killed hook. That is
// the safe direction by construction, and it is why this returns a duration
// instead of erroring: a misdeclared ceiling degrades to "do not block", never
// to "block past the kill".
func EnforceBudget(c provider.HookCeiling) time.Duration {
	return c.Gating - HookBudgetMargin
}

// Evaluator obtains the authoritative verdict for a gated tool call: one
// bounded, synchronous /evaluate round-trip, run before the tool does.
//
// It was Tier2, the upper half of a two-tier scheme whose lower half evaluated
// policy locally. ADR-0017 deleted the tiers rather than the escalation, so what
// remains is not "tier 2" of anything — it is the evaluation.
//
// The two providers differ in exactly one value — the wall-clock ceiling the
// tool kills the hook at. That is now declared through the SPI
// (provider.HookCeiling) rather than hardcoded per adapter, so the engine
// derives its own budget and no adapter carries a timeout the engine cannot see.
type Evaluator struct {
	// Ceiling is the provider's declared hook-kill limit. The whole-hook
	// budget derives from it via EnforceBudget: the local step, the evaluation
	// and any approval hold run sequentially, and their independently-clamped
	// budgets must never jointly push the hook past the kill (which would fail
	// open and defeat a fail-closed org).
	Ceiling provider.HookCeiling
	// MaxTimeout clamps the configured per-evaluation budget.
	MaxTimeout time.Duration
	// NewClient builds the control-plane transport for the evaluation and for
	// the approval hold that can follow it.
	NewClient func(*log.Logger) (Governor, error)

	// OnDelivered, when set, is called once the escalation's POST has returned
	// without a transport error — the point at which core has STORED this
	// event. A caller holding an observe copy of the same event uses it to drop
	// that copy: the two derive one event_id by design, core does not dedupe
	// developer events on it, so a second copy is a second stored row and a
	// second Merkle leaf for one real tool call.
	//
	// It is keyed on transport success, deliberately, NOT on the verdict. An
	// evaluation that came back unusable (EvaluationFailOpen "no verdict") or one
	// that came back REQUIRE_APPROVAL and led to a hold was still delivered and
	// still stored, so the observe copy is redundant in those cases too. Keying
	// it on the decision instead would miss both.
	//
	// It may run AFTER Escalate has already returned. A budget-exceeded
	// evaluation abandons the goroutine running the transport rather than
	// waiting for it, so the callback races the caller's own teardown and must
	// be safe to invoke concurrently with whatever reads what it sets.
	OnDelivered func()
}

// Governor is the control-plane transport the enforce path needs: escalate an
// event for an authoritative verdict, and read back where a filed approval
// stands. Satisfied by *client.Client; one interface rather than two because
// the hold always follows an evaluation over the same credentials.
type Governor interface {
	Emitter
	PollApproval(ctx context.Context, key client.ApprovalKey) (client.ApprovalStatus, error)
}

// Budget is the effective budget for one evaluation: the configured budget, but
// never more than the time remaining in the whole-hook budget after the local
// step ran. enforceStart is the instant the enforce block began.
//
// A non-positive remainder (the local step already consumed the ceiling) yields a
// non-positive budget, so Escalate fails open immediately rather than push the
// hook past the provider's timeout — the safe direction, by construction.
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

// remaining is what is left of the whole-hook budget. Every budget the gate
// hands out is clamped by it, so the sequential steps can never jointly overrun
// the provider's hook timeout however they are configured individually.
func (t Evaluator) remaining(enforceStart time.Time) time.Duration {
	return EnforceBudget(t.Ceiling) - time.Since(enforceStart)
}

// Escalate runs one bounded evaluation for an already-mapped event.
// Exceeding the budget degrades to fail-open rather than blocking the call.
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
	// A missing transport is a misconfiguration, but it must degrade like any
	// other fault rather than panic: this runs on every gated tool call since
	// ADR-0017, so a nil here would be a guaranteed crash on the enforce path
	// instead of the latent one it was when only shell and MCP escalated.
	// RunHook would recover it, but recovering a panic per tool call is not the
	// same as failing open.
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
	// The event is now stored server-side, whatever the verdict turns out to be.
	// Announce that before mapping the verdict, so no return path below can skip
	// it and leave a caller holding a duplicate observe copy.
	if t.OnDelivered != nil {
		t.OnDelivered()
	}
	return EvaluationDecision(eval)
}

// EvaluationFailOpen is the degraded escalation outcome: no real verdict, marked
// fail-open so the org's failure policy decides what happens next.
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

// DecisionTightens reports whether a decision would produce a deny/ask — i.e.
// whether it is an answer that already restricts the call.
func DecisionTightens(dec decision.Decision, c OutputContract) bool {
	d, _ := MapVerdict(dec.Evaluation, c)
	return d != ""
}

// ShouldEscalate reports whether a round-trip can still change the outcome.
// Normally it cannot once the local step has tightened, because the server can
// only ever be more restrictive — so evaluation fires when the local step would
// otherwise proceed.
//
// REQUIRE_APPROVAL is the one exception, and the reason this predicate exists
// separately from DecisionTightens: it is not a final answer but a QUESTION,
// and only the server can file it. Treating it as "already tightened" meant a
// locally-derived approval was rendered as a local prompt and never reached
// /evaluate — no governance_events row, no approval window, nothing for any
// approver to decide (E9 §3.4 Step 0). It therefore escalates like a proceed.
func ShouldEscalate(dec decision.Decision, c OutputContract) bool {
	if dec.Evaluation.Verdict == client.VerdictRequireApproval {
		return true
	}
	return !DecisionTightens(dec, c)
}

// resolveEvaluationTimeout is the per-evaluation budget.
//
// It reads no config. `tier2_timeout_ms` is deprecated and inert (ADR-0017):
// the real bound is the provider's declared hook ceiling, applied in Budget,
// and that is a correctness boundary rather than a tuning knob — latency and
// capacity are the platform's scope, not something tuned per machine. Honouring
// a per-machine override would let a developer shorten their own enforcement
// window.
func resolveEvaluationTimeout() time.Duration { return DefaultEvaluationTimeout }
