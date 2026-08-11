package hookflow

import (
	"context"
	"log"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// Decision sources for a Tier-2 escalation, distinct from the local bundle's so
// the audit can tell where a verdict came from.
const (
	SourceTier2         = "tier2:evaluate"
	SourceTier2FailOpen = "tier2:fail-open"
)

// DefaultTier2Timeout is the default budget for one escalation. It sits under
// the hook budget so a slow control plane degrades rather than trips the
// provider's hook kill.
const DefaultTier2Timeout = 3500 * time.Millisecond

// Tier2 is the synchronous escalation to the control plane for high-risk tool
// classes, used when the local Tier-1 decision would otherwise proceed.
//
// The two providers differ in exactly one value — the wall-clock ceiling for the
// whole hook, which Claude Code fixes at 4s under its 5s kill while Codex
// derives it from its installed hook timeout. That is now a field rather than a
// reason to carry a second copy of the escalation.
type Tier2 struct {
	// HookBudget is the whole-hook wall-clock ceiling: the T1 gate and a
	// possible T2 run sequentially, and their independently-clamped budgets
	// must never jointly push the hook past the provider's kill (which would
	// fail open and defeat a fail-closed org).
	HookBudget time.Duration
	// MaxTimeout clamps the configured per-escalation budget.
	MaxTimeout time.Duration
	// NewClient builds the control-plane transport for the escalation and for
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
	// evaluation that came back unusable (Tier2FailOpen "no verdict") or one
	// that came back REQUIRE_APPROVAL and led to a hold was still delivered and
	// still stored, so the observe copy is redundant in those cases too. Keying
	// it on the decision instead would miss both.
	OnDelivered func()
}

// Governor is the control-plane transport the enforce path needs: escalate an
// event for an authoritative verdict, and read back where a filed approval
// stands. Satisfied by *client.Client; one interface rather than two because
// the hold always follows an escalation over the same credentials.
type Governor interface {
	Emitter
	PollApproval(ctx context.Context, key client.ApprovalKey) (client.ApprovalStatus, error)
}

// Budget is the effective budget for an escalation: the configured T2 budget,
// but never more than the time remaining in HookBudget after the T1 gate ran.
// enforceStart is the instant the enforce block began.
//
// A non-positive remainder (T1 already consumed the ceiling) yields a
// non-positive budget, so Escalate fails open immediately rather than push the
// hook past the provider's timeout — the safe direction, by construction.
func (t Tier2) Budget(enforceStart time.Time, configured time.Duration) time.Duration {
	budget := configured
	if t.MaxTimeout > 0 && budget > t.MaxTimeout {
		budget = t.MaxTimeout
	}
	if rem := t.remaining(enforceStart); rem < budget {
		budget = rem
	}
	return budget
}

// remaining is what is left of the whole-hook ceiling. Every budget the gate
// hands out is clamped by it, so the tiers can never jointly overrun the
// provider's hook timeout however they are configured individually.
func (t Tier2) remaining(enforceStart time.Time) time.Duration {
	return t.HookBudget - time.Since(enforceStart)
}

// Escalate runs one bounded Tier-2 evaluation for an already-mapped event.
// Exceeding the budget degrades to fail-open rather than blocking the call.
func (t Tier2) Escalate(ctx context.Context, logger *log.Logger, ev client.DevEvent, budget time.Duration) decision.Decision {
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	resultCh := make(chan decision.Decision, 1)
	go func() { resultCh <- t.run(cctx, logger, ev) }()

	select {
	case dec := <-resultCh:
		return dec
	case <-cctx.Done():
		logger.Printf("tier-2 escalation degrading (budget %v exceeded)", budget)
		return Tier2FailOpen("tier-2 budget exceeded")
	}
}

func (t Tier2) run(cctx context.Context, logger *log.Logger, ev client.DevEvent) decision.Decision {
	cl, err := t.NewClient(logger)
	if err != nil {
		logger.Printf("tier-2 escalation degrading (client init): %v", err)
		return Tier2FailOpen("tier-2 client unavailable")
	}
	eval, err := cl.Emit(cctx, ev)
	if err != nil {
		logger.Printf("tier-2 escalation degrading (emit): %v", err)
		return Tier2FailOpen("tier-2 escalation undelivered")
	}
	// The event is now stored server-side, whatever the verdict turns out to be.
	// Announce that before mapping the verdict, so no return path below can skip
	// it and leave a caller holding a duplicate observe copy.
	if t.OnDelivered != nil {
		t.OnDelivered()
	}
	return Tier2Decision(eval)
}

// Tier2FailOpen is the degraded escalation outcome: no real verdict, marked
// fail-open so the org's failure policy decides what happens next.
func Tier2FailOpen(cause string) decision.Decision {
	return decision.Decision{
		Evaluation: client.Evaluation{Verdict: client.VerdictUnknown, Reason: cause},
		FailOpen:   true,
		Source:     SourceTier2FailOpen,
	}
}

func Tier2Decision(eval client.Evaluation) decision.Decision {
	if eval.Verdict == client.VerdictUnknown {
		return Tier2FailOpen("tier-2 /evaluate returned no verdict")
	}
	return decision.Decision{Evaluation: eval, Source: SourceTier2}
}

// DecisionTightens reports whether a decision would produce a deny/ask — i.e.
// whether it is an answer that already restricts the call.
func DecisionTightens(dec decision.Decision, c OutputContract) bool {
	d, _ := MapVerdict(dec.Evaluation, c)
	return d != ""
}

// ShouldEscalate reports whether a Tier-2 round-trip can still change the
// outcome. Normally it cannot once Tier-1 has tightened, because Tier-2 can
// only ever be more restrictive — so escalation fires when Tier-1 would
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

// KeepTighter picks between the Tier-1 and Tier-2 decisions: the Tier-2 answer
// wins, unless the escalation failed to deliver a real verdict and Tier-1 had
// already tightened — in which case the local decision stands.
//
// This became load-bearing with ShouldEscalate. While escalation ran only over
// a would-proceed Tier-1 there was nothing to lose; now that a local
// REQUIRE_APPROVAL escalates, a degraded round-trip would otherwise replace a
// deny/ask with VerdictUnknown and let the call through — enforcement loosening
// itself on an outage, which is exactly what the tighten-only invariant forbids.
func KeepTighter(t1, t2 decision.Decision, c OutputContract) decision.Decision {
	if t2.FailOpen && DecisionTightens(t1, c) {
		return t1
	}
	return t2
}

// resolveTier2Timeout reads the configured per-escalation budget. It is clamped
// only against overflow here; the real ceiling is Tier2.MaxTimeout, which is
// provider-derived and applied in Budget.
func resolveTier2Timeout() time.Duration {
	ms := devconfig.ResolveTimeoutMS(func(c devconfig.DevConfig) int { return c.Tier2TimeoutMS }, devconfig.EnvTier2Timeout)
	if ms <= 0 {
		return DefaultTier2Timeout
	}
	// Clamp in milliseconds before the multiply so a near-max-int64 value can
	// never overflow time.Duration.
	if const1h := int64(time.Hour / time.Millisecond); int64(ms) > const1h {
		return time.Hour
	}
	return time.Duration(ms) * time.Millisecond
}
