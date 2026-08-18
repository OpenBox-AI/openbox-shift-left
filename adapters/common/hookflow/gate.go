package hookflow

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"sync/atomic"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// EnforceTarget is the tool call being gated, as the shared gate needs to see
// it. Each adapter implements it over its own native hook event — the only
// provider-specific part of the gate is reading that event.
type EnforceTarget interface {
	// SessionID and ToolName identify the call for the stale gate and the audit.
	SessionID() string
	ToolName() string
	// ToolInput is the raw native tool input, used to reconstruct a redacted
	// replacement. Never egressed.
	ToolInput() json.RawMessage
	// HighRisk reports whether this class is shell execution or an MCP call.
	// The gate no longer consults it (ADR-0017 evaluates every class); it
	// survives for the adapters' own classification.
	HighRisk() bool
	// DecisionRequest builds the local decision request. Metadata axes only
	// (INV-2); content is carried solely for local redaction.
	DecisionRequest(localRedaction bool) decision.DecisionRequest
	// DevEvent maps the call for the inline evaluation, or reports !ok when it
	// cannot be mapped.
	//
	// redacted is the local secret-detection result for this call, or nil when
	// nothing was scanned or nothing matched. It is passed IN rather than looked
	// up because the ordering is the security property: the content this event
	// attaches must be the redacted body, and an implementation that cannot see
	// the redaction cannot honour that. A secret sent to the control plane lands
	// in governance event storage, which is the hardest place to purge it from.
	DevEvent(redacted *client.Content) (client.DevEvent, bool)
}

// EnforceGate is the synchronous pre-execution gate: the sequence a hook runs
// before the tool does.
//
// The individual steps were already shared; this shares their ORDER, which is
// the part a new tier or a reordering would otherwise have to be applied to
// twice. The order is load-bearing — stale gate before evaluation, failure
// policy between obtain and apply, evaluation only when the local step would proceed,
// audit after the decision is written — so it belongs in one place.
type EnforceGate struct {
	Contract  OutputContract
	Evaluator Evaluator
	// Record writes the durable audit line. Off the blocking path,
	// best-effort, never fails a tool call.
	Record func(dec decision.Decision, res ApplyResult)
	// SpoolObserve appends the gated call's observe copy to the local spool.
	//
	// The gate owns the timing because only the gate knows whether the Tier-2
	// escalation delivered this very event first. The adapter used to spool it
	// before running the gate, which stored the ActivityStarted TWICE for every
	// escalated call — the escalation POSTs the identical event, the pinned
	// mapper clock makes both derive one event_id, and core does not dedupe on
	// it. Two rows, two Merkle leaves, one real tool call.
	//
	// Called on exactly one path per Run: every exit where the escalation did
	// not deliver, including the stale-gate early return and every fail-open
	// degradation. Nil means the caller spooled its own copy already (the
	// non-gated hooks do).
	SpoolObserve func()
}

// Run gates one call. It is fail-open throughout (INV-3b): every fault
// degrades to proceed, and the gate can only ever add a deny/ask/redaction.
// It reports what the apply leg emitted, so a caller that owns other stdout
// writers on the same hook (the findings surface on a gated prompt) can keep
// the one-JSON-document-per-hook rule.
func (g EnforceGate) Run(ctx context.Context, logger *log.Logger, stdout io.Writer, t EnforceTarget) ApplyResult {
	// The observe copy rides whichever exit this gate takes: spooled unless the
	// escalation delivered the identical event. Deferred, and the flag is set
	// from inside the escalation's transport, so neither the early returns below
	// nor a degraded escalation can get this wrong — the default is to spool,
	// which is the direction that loses no telemetry.
	//
	// g is a value receiver, so wiring OnDelivered here mutates only this
	// invocation's copy: no shared state between concurrent hook processes.
	//
	// The flag is atomic because the two accesses can genuinely be concurrent.
	// Escalate runs the transport in its own goroutine and abandons it when the
	// budget expires; only its result-channel path carries a happens-before edge
	// back here, so on a budget-exceeded escalation the abandoned goroutine can
	// still be running — and calling OnDelivered — while this defer reads. A plain
	// bool made that a data race that -race reports, and left the read free to
	// observe a stale value.
	//
	// Reading "not delivered" while a POST is still in flight is not itself the
	// bug: an escalation we gave up on may or may not have been stored, and the
	// direction of failure is settled — spool, because a redundant copy is a bug
	// and a missing one is lost telemetry. What remains is the lost-200 window
	// (core committed the row, our client saw only the cancellation), which no
	// client-side change can close; server-side dedupe is still absent.
	if g.SpoolObserve != nil {
		var delivered atomic.Bool
		g.Evaluator.OnDelivered = func() { delivered.Store(true) }
		defer func() {
			if !delivered.Load() {
				g.SpoolObserve()
			}
		}()
	}

	// One wall clock for the whole gate (the local step plus the evaluation), so the
	// sequential budgets can never jointly exceed the provider's hook
	// timeout — which would fail open and defeat a fail-closed org.
	enforceStart := time.Now()

	// Local redaction gate: the tool body reaches the in-process detector only
	// under secret detection (default on) or content capture. With both off,
	// no body is scanned and no redaction is emitted — byte-identical to the
	// observe baseline.
	localRedaction := devconfig.ResolveSecretDetection() || devconfig.ResolveContentCapture()

	// The local step redacts; it does not decide (ADR-0017).
	local := NewDecider().Decide(ctx, t.DecisionRequest(localRedaction))

	// Evaluate. Unconditional, because there is no local verdict that could
	// make a round-trip pointless: every gated call is the server's to decide.
	//
	// The failure policy runs strictly AFTER this, and that ordering is now
	// load-bearing rather than stylistic. It used to sit between the local
	// verdict and the escalation, which was harmless while the local step
	// produced real verdicts — but a local step that always reports "no verdict"
	// turns an early ApplyFailurePolicy into an immediate synthesized HALT under
	// fail_closed, which then reads as "already tightened" and suppresses the
	// evaluation entirely. A fail-closed org would deny every gated call without
	// ever asking. C1 caught exactly that.
	dec, key := g.escalate(ctx, logger, t, local.RedactedContent, enforceStart)

	// Carry the local redaction onto the decision so a redact-and-continue still
	// applies on the proceed path.
	dec.RedactedContent = local.RedactedContent
	dec.RedactionCategories = local.RedactionCategories

	// On an evaluation outage under fail-closed this synthesizes a HALT so the
	// unchanged apply cascade denies. A real verdict passes through untouched.
	policy := ResolveFailurePolicy()
	dec = ApplyFailurePolicy(dec, policy)

	// A server REQUIRE_APPROVAL is a filed request, not a final answer, so hold
	// for whoever is going to decide it. The hook blocks the tool call while it
	// runs either way — the only question is whether it spends that time asking.
	//
	// Only a SERVER verdict is held for: a degraded evaluation has filed no
	// record, so holding on it would spend the whole budget on not-founds and
	// then deny.
	if dec.Evaluation.Verdict == client.VerdictRequireApproval && dec.Source == SourceEvaluate {
		dec = g.awaitApproval(ctx, logger, t, dec, key, enforceStart)
	}

	// A HALT the control plane actually returned terminates the session, not
	// just this call (plan 260818-1714). Marked AFTER the hold, so a hold
	// outcome — which re-sources the decision to approval:decided or
	// approval:undecided — can never qualify, and a synthesized fail-closed
	// HALT (FailOpen=true) stays a per-call deny.
	if dec.Evaluation.Verdict == client.VerdictHalt && dec.Source == SourceEvaluate && !dec.FailOpen {
		dec.SessionHalt = true
	}

	LogEnforceDecision(logger, t.ToolName(), dec, policy)
	// Apply the decision, plus the proceed-path redaction. The audit runs after
	// the decision is written, off the blocking path.
	res := ApplyDecision(stdout, dec, localRedaction, t.ToolInput(), g.Contract)
	// Latch only what the provider actually expressed: an emitted session stop.
	// A contract with no session-stop lever (Codex) renders a HALT as its
	// per-call deny, so it never accumulates latch state its hooks would not
	// consult.
	if res.Decision == DecisionHalt {
		WriteSessionHalt(logger, t.SessionID(), dec.Evaluation)
	}
	g.Record(dec, res)
	return res
}

// escalate runs one Tier-2 round-trip and returns the poll key for the approval
// it may have filed. The key is derived from the SAME event that was escalated,
// so a hold can only ever address the row this call created.
func (g EnforceGate) escalate(ctx context.Context, logger *log.Logger, t EnforceTarget, redacted *client.Content, enforceStart time.Time) (decision.Decision, client.ApprovalKey) {
	ev, ok := t.DevEvent(redacted)
	if !ok {
		return EvaluationFailOpen("event not mappable"), client.ApprovalKey{}
	}
	dec := g.Evaluator.Escalate(ctx, logger, ev, g.Evaluator.Budget(enforceStart, resolveEvaluationTimeout()))
	return dec, client.ApprovalKeyFor(ev)
}

// awaitApproval runs the bounded hold and folds the outcome back into the
// gate's decision. An unanswered request denies (OD-E9-1): never a silent allow
// in enforce mode, and never the provider's own approval prompt, which would
// ask the developer to approve their own filed request.
func (g EnforceGate) awaitApproval(ctx context.Context, logger *log.Logger, t EnforceTarget, dec decision.Decision, key client.ApprovalKey, enforceStart time.Time) decision.Decision {
	if !key.Valid() {
		return ApprovalUndecided(dec, "— this call cannot be tied to an approval record")
	}
	// Mark the request BEFORE holding, so the rewake watcher — a sibling
	// background process that started at the same instant — learns within its
	// short grace period that this call has an approval worth outliving the
	// hook for. Marking after the hold would make every watcher wait out the
	// whole hold just to discover the usual answer: nothing to do.
	RecordPendingApproval(logger, key, t.ToolName())

	answered, ok := g.Evaluator.AwaitApproval(ctx, logger, key, enforceStart)
	if !ok {
		// Leave the marker standing: the watcher owns the tail from here.
		return ApprovalUndecided(dec, "within this hook's budget")
	}
	// Claim the outcome so the watcher does not also announce it.
	ClaimPendingApproval(key)
	// An approved call still proceeds through the Tier-1 redact-and-continue.
	answered.RedactedContent = dec.RedactedContent
	answered.RedactionCategories = dec.RedactionCategories
	return answered
}
