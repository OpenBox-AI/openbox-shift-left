package hookflow

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"sync/atomic"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// EnforceTarget is the tool call being gated, as the shared gate needs to see
// it.
type EnforceTarget interface {
	// SessionID and ToolName identify the call for the stale gate and the audit.
	SessionID() string
	ToolName() string
	// ToolInput is the raw native tool input, used to reconstruct a redacted
	// replacement. Never egressed.
	ToolInput() json.RawMessage
	// HighRisk reports whether this class is shell execution or an MCP call.
	HighRisk() bool
	// DecisionRequest builds the local decision request.
	DecisionRequest(localRedaction bool) decision.DecisionRequest
	// DevEvent maps the call for the inline evaluation, or reports !ok when it
	// cannot be mapped.
	DevEvent(redacted *client.Content) (client.DevEvent, bool)
}

// EnforceGate is the synchronous pre-execution gate: the sequence a hook runs
// before the tool does.
type EnforceGate struct {
	Contract  OutputContract
	Evaluator Evaluator
	// Record writes the durable audit line. Off the blocking path, best-effort,
	// never fails a tool call.
	Record func(dec decision.Decision, res ApplyResult)
	// SpoolObserve appends the gated call's observe copy to the local spool,
	// as this call's OWN first delivery attempt: only for
	// EscalationNotAttempted (no client, or no budget left -- the escalation
	// POST was never sent).
	SpoolObserve func()
	// SpoolObserveHead appends the gated call's observe copy directly to the
	// session's head file, ahead of its tail: only for EscalationUnanswered
	// (the escalation WAS sent, but this call's own budget ran out before
	// core answered). Never invoked alongside SpoolObserve for the same
	// call; an explicit non-acceptance (EscalationSettled, already latched
	// by HaltOnDeliveryFailure) invokes neither.
	SpoolObserveHead func()
	// RunID is the run this call belongs to (phase 08, R11/V14): what
	// WriteSessionHalt latches on. "" (the zero value) falls back to
	// t.SessionID(), the same selection client.runIDFor makes, and is what
	// every existing caller gets -- Codex never sets this and its HALT never
	// reaches WriteSessionHalt anyway (see the comment below), so this field
	// changes nothing for it.
	RunID string
}

// runID is the same selection client.runIDFor makes, computed here rather
// than added to EnforceTarget: adding a method there would touch every
// implementation across both providers for a value only claude-code's
// hookrun.go currently has a run record to supply.
func (g EnforceGate) runID(t EnforceTarget) string {
	if g.RunID != "" {
		return g.RunID
	}
	return t.SessionID()
}

// Run gates one call.
func (g EnforceGate) Run(ctx context.Context, logger *log.Logger, stdout io.Writer, t EnforceTarget) ApplyResult {
	// Zero value EscalationSettled: nothing to spool unless the escalation
	// itself reports otherwise. atomic because run's own goroutine (the one
	// Escalate spawns) can still be writing this after Escalate has already
	// returned via its own timeout branch.
	var outcome atomic.Int32
	g.Evaluator.OnOutcome = func(o EscalationOutcome) { outcome.Store(int32(o)) }
	defer func() {
		switch EscalationOutcome(outcome.Load()) {
		case EscalationNotAttempted:
			if g.SpoolObserve != nil {
				g.SpoolObserve()
			}
		case EscalationUnanswered:
			if g.SpoolObserveHead != nil {
				g.SpoolObserveHead()
			}
		}
	}()

	enforceStart := time.Now()

	localRedaction := devconfig.ResolveSecretDetection() || devconfig.ResolveContentCapture()

	local := NewDecider().Decide(ctx, t.DecisionRequest(localRedaction))

	// The failure policy runs strictly after this, and that ordering is now load-
	// bearing rather than stylistic.
	dec, key := g.escalate(ctx, logger, t, local.RedactedContent, enforceStart)

	dec.RedactedContent = local.RedactedContent
	dec.RedactionCategories = local.RedactionCategories

	policy := ResolveFailurePolicy()
	dec = ApplyFailurePolicy(dec, policy)

	if dec.Evaluation.Verdict == client.VerdictRequireApproval && dec.Source == SourceEvaluate {
		dec = g.awaitApproval(ctx, logger, t, dec, key, enforceStart)
	}

	if dec.Evaluation.Verdict == client.VerdictHalt && dec.Source == SourceEvaluate && !dec.FailOpen {
		dec.SessionHalt = true
	}

	LogEnforceDecision(logger, t.ToolName(), dec, policy)
	res := ApplyDecision(stdout, dec, localRedaction, t.ToolInput(), g.Contract)
	// Only a contract that RENDERS a session stop reports DecisionHalt back, so
	// only such a contract latches. Both providers deliberately keep that to one
	// contract each -- the prompt contract, the single surface either vendor
	// gives a session-stop lever on. Every tool contract folds HALT into its
	// per-call refusal and returns that instead, so no tool call can latch.
	//
	// The read side is wider than the write side on purpose: every gated class
	// consults the latch, or a session halted at the prompt would keep running
	// tools.
	if res.Decision == DecisionHalt {
		WriteSessionHalt(logger, g.runID(t), dec.Evaluation)
	}
	g.Record(dec, res)
	return res
}

func (g EnforceGate) escalate(ctx context.Context, logger *log.Logger, t EnforceTarget, redacted *client.Content, enforceStart time.Time) (decision.Decision, client.ApprovalKey) {
	ev, ok := t.DevEvent(redacted)
	if !ok {
		// No escalation POST exists to send at all: the same "nothing was
		// ever attempted" outcome a missing client or an exhausted budget
		// reports, so the caller's own (separately mapped) observe copy
		// still gets spooled as this event's first and only attempt rather
		// than silently dropped.
		g.Evaluator.reportOutcome(EscalationNotAttempted)
		return EvaluationFailOpen("event not mappable"), client.ApprovalKey{}
	}
	dec := g.Evaluator.Escalate(ctx, logger, ev, g.Evaluator.Budget(enforceStart, DefaultEvaluationTimeout))
	return dec, client.ApprovalKeyFor(ev)
}

// awaitApproval an unanswered request denies (OD-E9-1): never a silent allow
// in enforce mode, and never the provider's own approval prompt, which would
// ask the developer to approve their own filed request.
func (g EnforceGate) awaitApproval(ctx context.Context, logger *log.Logger, t EnforceTarget, dec decision.Decision, key client.ApprovalKey, enforceStart time.Time) decision.Decision {
	if !key.Valid() {
		return ApprovalUndecided(dec, "- this call cannot be tied to an approval record")
	}
	RecordPendingApproval(logger, key, t.ToolName())

	answered, ok := g.Evaluator.AwaitApproval(ctx, logger, key, enforceStart)
	if !ok {
		return ApprovalUndecided(dec, "within this hook's budget")
	}
	ClaimPendingApproval(key)
	answered.RedactedContent = dec.RedactedContent
	answered.RedactionCategories = dec.RedactionCategories
	return answered
}
