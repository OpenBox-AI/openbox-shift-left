package hookflow

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// The bounded hold (E9 §2.2 Tier 1).
//
// A REQUIRE_APPROVAL verdict from the control plane is not an answer, it is a
// filed request: core has minted an approval window and someone — an autonomous
// approver, or a human already watching the dashboard — is expected to decide
// it. The hook is going to hold anyway, because the tool call is blocked until
// it returns, so it may as well spend that time asking. A server-side approver
// deciding in ~100ms is answered on the FIRST poll.
//
// That is the whole reason this needs no local socket and no second install
// (ADR-0006 stands): the hook is still a short-lived process making one
// authenticated HTTP call, exactly as it does for /evaluate.
//
// The hold is bounded three ways at once, and takes the tightest: the org's
// configured budget, whatever is left of the provider's hook timeout, and the
// approval window core itself minted. Past all of those the request is
// undecided, and an undecided approval DENIES — never a silent allow (OD-E9-1).

// DefaultApprovalHold is the default bounded wait for a filed approval. It sits
// under the raised gate-hook ceiling with room for the apply and audit that
// follow, and is well past the sub-second latency of a server-side approver —
// so the common case is answered many times over, and a human tail is denied
// rather than waited on.
const DefaultApprovalHold = 20 * time.Second

// approvalPollInterval is the cadence inside the hold. Fast enough that an
// autonomous decision is picked up essentially the moment it lands, slow enough
// that a whole hold is a few dozen requests rather than a few thousand.
const approvalPollInterval = 500 * time.Millisecond

// SourceApprovalDecided marks a decision that came from an approver answering
// during the hold — distinct from the escalation that filed the request, so the
// audit can tell "the server said approval was needed" from "someone decided".
const SourceApprovalDecided = "approval:decided"

// AwaitApproval holds the tool call while a filed approval is decided, and
// reports the answer. ok is false when the hold ended with the request still
// undecided — the caller turns that into a deny (see ApprovalUndecided).
//
// It never panics a hook and never runs past its budget: every fault (no
// transport, an unreachable control plane, an unparseable status) is logged and
// re-tried at the poll cadence until the budget is spent, because a transport
// blip during a hold is not evidence that the approval was refused.
func (t Evaluator) AwaitApproval(ctx context.Context, logger *log.Logger, key client.ApprovalKey, enforceStart time.Time) (decision.Decision, bool) {
	budget := t.HoldBudget(enforceStart, resolveApprovalHold())
	if budget <= 0 {
		return decision.Decision{}, false
	}
	cl, err := t.NewClient(logger)
	if err != nil {
		logger.Printf("approval hold skipped (client init): %v", err)
		return decision.Decision{}, false
	}

	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	logger.Printf("approval hold: waiting up to %v for a decision", budget)

	tick := time.NewTicker(approvalPollInterval)
	defer tick.Stop()
	for {
		st, err := cl.PollApproval(cctx, key)
		switch {
		case err == nil && st.Decided():
			logger.Printf("approval hold: decided verdict=%s", st.Verdict)
			return approvalDecision(st), true
		case err == nil && windowClosed(st):
			// The window core minted has passed. Nothing will decide this
			// request now, so stop rather than spend the rest of the budget.
			logger.Printf("approval hold: window closed undecided")
			return decision.Decision{}, false
		case err != nil && !errors.Is(err, client.ErrApprovalNotFound) && cctx.Err() == nil:
			// Not-found is normal early on (the row may not have landed yet);
			// anything else is a real fault, and the cadence is the retry.
			//
			// The cctx guard matters for legibility: the budget expiring cancels
			// whatever poll is in flight, and reporting that as a degraded poll
			// made every ordinary exhausted hold end on what looked like a
			// transport error.
			logger.Printf("approval hold: poll degraded: %v", err)
		}
		select {
		case <-cctx.Done():
			return decision.Decision{}, false
		case <-tick.C:
		}
	}
}

// HoldBudget is the effective budget for an approval hold: the configured hold,
// never more than what is left of the whole-hook ceiling after the gate's
// earlier work. A non-positive result means there is no room to hold, so the
// caller denies immediately rather than overrun the provider's hook timeout —
// which would be killed and fail open, defeating a fail-closed org.
func (t Evaluator) HoldBudget(enforceStart time.Time, configured time.Duration) time.Duration {
	if rem := t.remaining(enforceStart); rem < configured {
		return rem
	}
	return configured
}

// windowClosed reports that the approval window core minted has passed. A
// record with no window is not treated as closed: that means core sent no
// expiry, not that the request expired.
func windowClosed(st client.ApprovalStatus) bool {
	return !st.ExpiresAt.IsZero() && !time.Now().Before(st.ExpiresAt)
}

// approvalDecision lifts a decided approval into the ordinary decision the
// apply cascade already handles — approve becomes ALLOW and proceeds, reject
// becomes HALT and denies, through exactly the same MapVerdict path as any
// other verdict. Nothing about approval is special downstream of here.
func approvalDecision(st client.ApprovalStatus) decision.Decision {
	return decision.Decision{
		Evaluation: client.Evaluation{
			Verdict:           st.Verdict,
			Reason:            st.Reason,
			GovernanceEventID: st.EventID,
		},
		Source: SourceApprovalDecided,
	}
}

// ApprovalUndecided converts a filed-but-unanswered approval into a deny that
// names the reference, so the developer (and the model, which sees a denial
// reason) can say what is being waited on and work elsewhere meanwhile.
//
// Denying rather than falling through to the provider's approval prompt is
// deliberate. The prompt asks the DEVELOPER to approve their own request, on
// the machine that made it — a convenience control, not four-eyes (E9 §3.7).
// Once a request has actually been filed with the control plane, the only
// principal that may answer it is an approver.
func ApprovalUndecided(dec decision.Decision, cause string) decision.Decision {
	reason := "action requires approval and none was decided " + cause
	if ref := dec.Evaluation.ApprovalRef(); ref != "" {
		reason += " (approval: " + ref + ")"
	}
	dec.Evaluation.Verdict = client.VerdictHalt
	dec.Evaluation.Reason = reason
	return dec
}

// resolveApprovalHold reads the org's configured hold. Clamping against the
// provider's hook timeout is HoldBudget's job, as with every other budget knob.
func resolveApprovalHold() time.Duration {
	ms := devconfig.ResolveTimeoutMS(func(c devconfig.DevConfig) int { return c.ApprovalHoldMS }, devconfig.EnvApprovalHold)
	if ms <= 0 {
		return DefaultApprovalHold
	}
	// Clamp in milliseconds before the multiply so a near-max-int64 value can
	// never overflow time.Duration.
	if oneHourMS := int64(time.Hour / time.Millisecond); int64(ms) > oneHourMS {
		return time.Hour
	}
	return time.Duration(ms) * time.Millisecond
}
