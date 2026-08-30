package hookflow

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// Past all of those the request is undecided, and an undecided approval
// denies; never a silent allow (OD-E9-1).

// DefaultApprovalHold is the default bounded wait for a filed approval.
const DefaultApprovalHold = 20 * time.Second

const approvalPollInterval = 500 * time.Millisecond

// SourceApprovalDecided marks a decision that came from an approver answering
// during the hold; distinct from the escalation that filed the request, so the
// audit can tell "the server said approval was needed" from "someone decided".
const SourceApprovalDecided = "approval:decided"

// SourceApprovalUndecided marks the deny synthesized when a filed approval was
// never answered within the hold. A HALT with this source denies one call; it
// must never terminate the session.
const SourceApprovalUndecided = "approval:undecided"

// AwaitApproval holds the tool call while a filed approval is decided, and
// reports the answer. Ok is false when the hold ended with the request still
// undecided; the caller turns that into a deny (see ApprovalUndecided).
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
			logger.Printf("approval hold: window closed undecided")
			return decision.Decision{}, false
		case err != nil && !errors.Is(err, client.ErrApprovalNotFound) && cctx.Err() == nil:
			logger.Printf("approval hold: poll degraded: %v", err)
		}
		select {
		case <-cctx.Done():
			return decision.Decision{}, false
		case <-tick.C:
		}
	}
}

// HoldBudget is the effective budget for an approval hold: the configured
// hold, never more than what is left of the whole-hook ceiling after the
// gate's earlier work.
func (t Evaluator) HoldBudget(enforceStart time.Time, configured time.Duration) time.Duration {
	if rem := t.remaining(enforceStart); rem < configured {
		return rem
	}
	return configured
}

func windowClosed(st client.ApprovalStatus) bool {
	return !st.ExpiresAt.IsZero() && !time.Now().Before(st.ExpiresAt)
}

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
func ApprovalUndecided(dec decision.Decision, cause string) decision.Decision {
	reason := "action requires approval and none was decided " + cause
	if ref := dec.Evaluation.ApprovalRef(); ref != "" {
		reason += " (approval: " + ref + ")"
	}
	dec.Evaluation.Verdict = client.VerdictHalt
	dec.Evaluation.Reason = reason
	dec.Source = SourceApprovalUndecided
	return dec
}

func resolveApprovalHold() time.Duration {
	ms := devconfig.ResolveTimeoutMS(func(c devconfig.DevConfig) int { return c.ApprovalHoldMS }, devconfig.EnvApprovalHold)
	if ms <= 0 {
		return DefaultApprovalHold
	}
	if oneHourMS := int64(time.Hour / time.Millisecond); int64(ms) > oneHourMS {
		return time.Hour
	}
	return time.Duration(ms) * time.Millisecond
}
