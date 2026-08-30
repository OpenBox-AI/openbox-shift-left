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
	// SpoolObserve appends the gated call's observe copy to the local spool.
	SpoolObserve func()
}

// Run gates one call.
func (g EnforceGate) Run(ctx context.Context, logger *log.Logger, stdout io.Writer, t EnforceTarget) ApplyResult {
	if g.SpoolObserve != nil {
		var delivered atomic.Bool
		g.Evaluator.OnDelivered = func() { delivered.Store(true) }
		defer func() {
			if !delivered.Load() {
				g.SpoolObserve()
			}
		}()
	}

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
	// A contract with no session-stop lever (Codex) renders a HALT as its per-
	// call deny, so it never accumulates latch state its hooks would not consult.
	if res.Decision == DecisionHalt {
		WriteSessionHalt(logger, t.SessionID(), dec.Evaluation)
	}
	g.Record(dec, res)
	return res
}

func (g EnforceGate) escalate(ctx context.Context, logger *log.Logger, t EnforceTarget, redacted *client.Content, enforceStart time.Time) (decision.Decision, client.ApprovalKey) {
	ev, ok := t.DevEvent(redacted)
	if !ok {
		return EvaluationFailOpen("event not mappable"), client.ApprovalKey{}
	}
	dec := g.Evaluator.Escalate(ctx, logger, ev, g.Evaluator.Budget(enforceStart, resolveEvaluationTimeout()))
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
