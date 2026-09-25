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
	// core answered) AND this call's own drain step (below) left the queue
	// empty behind it -- otherwise (Queue set but something is still queued)
	// SpoolObserve is used instead, so this call's own event never overtakes
	// an undrained predecessor still waiting ahead of it in the same
	// session. Never invoked alongside SpoolObserve for the same call; an
	// explicit non-acceptance (EscalationSettled, already latched by
	// HaltOnDeliveryFailure) invokes neither.
	SpoolObserveHead func()
	// RunID is the run this call belongs to (phase 08, R11/V14): what
	// WriteSessionHalt latches on. "" (the zero value) falls back to
	// t.SessionID(), the same selection client.runIDFor makes, and is what
	// every existing caller gets -- Codex never sets this and its HALT never
	// reaches WriteSessionHalt anyway (see the comment below), so this field
	// changes nothing for it.
	RunID string
	// Queue is this session's own spool engine. When set, Run drains
	// whatever fits in this call's own slack -- the time left over once the
	// escalation's own budget is reserved -- before escalating, under this
	// session's own stripe (the same lock an inline SessionStart/SessionEnd
	// attempt takes), so nothing already queued for this session reaches
	// core after this call's own event does. Nil (every gate literal before
	// this field existed) skips the drain AND the post-drain latch re-read
	// below entirely: behavior is byte-identical to a gate that has never
	// heard of a queue.
	Queue *Engine
	// OnHalted renders a run this call's own drain step discovered was
	// halted: a delivery failure, or a HALT verdict, recorded on one of the
	// events this call just drained -- something the PRE-gate latch check
	// (which runs before any drain ever happens) could not yet have seen.
	// The adapter wires the SAME closure that check already uses to replay a
	// halt, so there is one implementation of "how a halted run answers a
	// gated call," not two. Never called when Queue is nil.
	OnHalted func(SessionHaltInfo) ApplyResult
	// ForceFlush spawns the adapter's own detached flusher when this call's
	// own drain step leaves anything still queued for the session -- cut
	// short by its own slack, a stripe already held elsewhere, or an
	// unbuildable client -- so a backlog does not sit out the ordinary
	// realtime debounce window waiting for the next hook to nudge it. Never
	// called when Queue is nil.
	ForceFlush func()
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
	// The spool is keyed by SESSION id (every Append/Observe/inlineAttempt
	// call already keys on it, never the run id); captured once so both the
	// drain step below and the deferred dispatch read the same value.
	sessionID := t.SessionID()
	defer func() {
		switch EscalationOutcome(outcome.Load()) {
		case EscalationNotAttempted:
			if g.SpoolObserve != nil {
				g.SpoolObserve()
			}
		case EscalationUnanswered:
			// Re-checked NOW, at dispatch time, never from a snapshot taken
			// before the escalation ran: a lane daemon shares this session's
			// own spool and can append a new event for it WHILE this call's
			// own escalation is still in flight, and putting this call's own
			// event at the head would let it overtake that append. A stale
			// "the drain left nothing behind" read taken before escalate()
			// even started could not see that.
			if g.Queue != nil && g.Queue.Spool.PendingCount(sessionID) > 0 {
				if g.SpoolObserve != nil {
					g.SpoolObserve()
				}
			} else if g.SpoolObserveHead != nil {
				g.SpoolObserveHead()
			}
		}
	}()

	enforceStart := time.Now()

	localRedaction := devconfig.ResolveSecretDetection() || devconfig.ResolveContentCapture()

	local := NewDecider().Decide(ctx, t.DecisionRequest(localRedaction))

	// One client for this whole run -- drain, escalation, approval hold
	// (whichever of those three actually run) -- built once here rather
	// than separately by each step.
	cl, clientErr := g.Evaluator.buildClient(logger)

	if g.Queue != nil {
		// The halt latch is keyed by the RUN id (WriteSessionHalt/
		// SessionHalted's own contract, which differs from the session id
		// once a run has been bumped) -- a different identifier from the
		// spool's own session-id key just above, and this is the one place
		// both are read together, so neither may be used for the other's
		// job.
		runID := g.runID(t)
		if clientErr == nil {
			g.drain(ctx, logger, sessionID, cl, enforceStart)
		}
		if g.Queue.Spool.PendingCount(sessionID) > 0 && g.ForceFlush != nil {
			g.ForceFlush()
		}
		// A failure or a HALT verdict this call's own drain just found (or
		// that landed on this run through any other path since the
		// pre-gate check ran) answers this call without ever escalating:
		// the latch is the decided state, and asking /evaluate again would
		// be asking a question governance already answered.
		if info, halted := SessionHalted(runID); halted {
			if g.OnHalted != nil {
				return g.OnHalted(info)
			}
			// No adapter-specific replay wired: render a generic
			// fail-closed deny carrying the latch's own reason directly,
			// through the same apply path every other decision in this Run
			// goes through, rather than silently falling through to
			// re-ask an already-decided question.
			dec, res := SessionHaltReplay(logger, stdout, info, localRedaction, t.ToolInput(), t.ToolName(), g.Contract)
			g.Record(dec, res)
			return res
		}
	}

	// The failure policy runs strictly after this, and that ordering is now load-
	// bearing rather than stylistic.
	dec, key := g.escalate(ctx, logger, t, cl, clientErr, local.RedactedContent, enforceStart)

	dec.RedactedContent = local.RedactedContent
	dec.RedactionCategories = local.RedactionCategories

	policy := ResolveFailurePolicy()
	dec = ApplyFailurePolicy(dec, policy)

	if dec.Evaluation.Verdict == client.VerdictRequireApproval && dec.Source == SourceEvaluate {
		dec = g.awaitApproval(ctx, logger, t, cl, clientErr, dec, key, enforceStart)
	}

	if dec.Evaluation.Verdict == client.VerdictHalt && dec.Source == SourceEvaluate && !dec.FailOpen {
		dec.SessionHalt = true
	}

	LogEnforceDecision(logger, t.ToolName(), dec, policy)
	res := ApplyDecision(stdout, dec, localRedaction, t.ToolInput(), g.Contract)
	// Only a contract that RENDERS a session stop reports DecisionHalt back, so
	// only such a contract latches -- and the two providers do not agree on
	// which contracts that is. Claude Code's own TOOL contract (PreToolUse)
	// DOES render one (continue:false + stopReason) and returns the HALT
	// literal itself, so a tool-call HALT latches there too
	// (TestSessionHaltConformance "C27 tool HALT denies, stops the session,
	// and latches"). Codex's tool contracts (PreToolUse, PermissionRequest)
	// fold a HALT into an ordinary per-call deny instead (outputcontract.go,
	// permissiongate.go) -- Codex has exactly one contract with a stop lever,
	// its own UserPromptSubmit prompt contract, and that is the only Codex
	// surface where a HALT latches.
	//
	// The read side is wider than the write side on purpose: every gated class
	// consults the latch, or a session halted on one contract would keep
	// running calls a different contract renders.
	if res.Decision == DecisionHalt {
		WriteSessionHalt(logger, g.runID(t), dec.Evaluation)
	}
	g.Record(dec, res)
	return res
}

// MaxStripeWait caps how long a gated call's own drain step (below) will
// ever wait to ACQUIRE a busy stripe -- a live detached flusher or lane
// daemon may be mid-delivery on one slow event, holding this session's own
// stripe for as long as its own attempt takes -- so a gated call is never
// held hostage by someone else's slow event: a measured stripe-held gate
// wall p95 above 5s is the pre-decided trigger this cap answers. It bounds
// ONLY the wait to take the lock (DrainOptions.LockWait), never the drain
// PASS itself: once this call's own drain acquires an uncontended stripe, it
// still gets the full slack to clear a real backlog (the "try hardest not to
// let an event slip through" governing principle), never cut short at this
// same 5s regardless of how large that backlog is.
const MaxStripeWait = 5 * time.Second

// GateDrainAttemptTimeout bounds ONE drain attempt inside a gated call's own
// slack -- deliberately shorter than DefaultEvaluationTimeout (the gate's own
// escalation budget), not equal to it. An attempt this short that goes
// unanswered is requeued for the 30s background drainers
// (RequeueUnanswered), so a short attempt costs this call nothing beyond the
// time it waited; only a live-but-slower-than-3.5s core pays a repeated toll
// (one doomed 3.5s attempt per gated call, with the backlog only actually
// clearing via the 30s drainers). It must never exceed
// DefaultEvaluationTimeout: the contended worst case is GateDrainAttemptTimeout
// + DefaultEvaluationTimeout + MaxStripeWait (18.5s today), and matching the
// two would push it to 25s.
const GateDrainAttemptTimeout = 3500 * time.Millisecond

// drain runs this call's own queue-drain step: whatever fits in the slack
// left over once the escalation's own budget (DefaultEvaluationTimeout, or
// less under MaxTimeout/remaining) is reserved out of what remains of the
// hook's own ceiling, waiting at most MaxStripeWait to take this session's
// own stripe (this call owns it for its own duration, the same as an inline
// SessionStart/SessionEnd attempt, once acquired) and requeuing -- never
// failing -- an attempt this call's own window could not get an answer for
// (RequeueUnanswered: the identical unanswered-vs-explicit-failure
// discriminator the escalation step below reuses through EscalateWith). A
// stripe still busy past MaxStripeWait, or an exhausted slack, both leave the
// backlog exactly where it was: unattempted, never a failure. A drain that
// could not even start one attempt (its own slack already below
// GateDrainAttemptTimeout) touches nothing at all -- DrainSession's own
// contract -- so calling this with a slack of, say, a few milliseconds costs
// nothing beyond the call itself.
//
// The attempt itself is bounded by GateDrainAttemptTimeout, NOT by
// DefaultEvaluationTimeout: the two are deliberately different budgets now
// (see GateDrainAttemptTimeout's own comment) -- a short, disposable drain
// attempt inside this call's own slack, versus the full budget this call's
// own escalation gets afterward.
func (g EnforceGate) drain(ctx context.Context, logger *log.Logger, sessionID string, cl Governor, enforceStart time.Time) {
	slack := g.Evaluator.remaining(enforceStart) - g.Evaluator.Budget(enforceStart, DefaultEvaluationTimeout)
	// AttemptTimeout is NEVER clamped down to slack: a pass never starts an
	// attempt it cannot finish (DrainSession's own contract), so if slack
	// itself is shorter than one drain attempt, the right answer is to skip
	// entirely -- not to shrink the attempt bound to match slack exactly,
	// which would make DrainSession's "don't start what can't finish" guard
	// compare slack to itself: a comparison with no real margin, decided by a
	// few microseconds of scheduling on either side of the boundary, that
	// lets a doomed-to-time-out attempt fire (and take the stripe lock for
	// it) roughly as often as it correctly skips. Checked here, before ever
	// calling DrainSession, so a slack too small for one attempt never even
	// reaches for the lock.
	if slack < GateDrainAttemptTimeout {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, slack)
	defer cancel()
	n, err := g.Queue.DrainSession(dctx, sessionID, cl, DrainOptions{
		Mode:              Block,
		AttemptTimeout:    GateDrainAttemptTimeout,
		RequeueUnanswered: true,
		LockWait:          MaxStripeWait,
	})
	if n > 0 {
		logger.Printf("gate drain: drained %d event(s) in %v", n, time.Since(enforceStart))
	}
	if err != nil {
		logger.Printf("gate drain ended early: %v", err)
	}
}

func (g EnforceGate) escalate(ctx context.Context, logger *log.Logger, t EnforceTarget, cl Governor, clientErr error, redacted *client.Content, enforceStart time.Time) (decision.Decision, client.ApprovalKey) {
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
	key := client.ApprovalKeyFor(ev)
	if clientErr != nil {
		logTransportFailure(logger, clientErr)
		g.Evaluator.reportOutcome(EscalationNotAttempted)
		return EvaluationFailOpen("control plane unreachable"), key
	}
	budget := g.Evaluator.Budget(enforceStart, DefaultEvaluationTimeout)
	dec := g.Evaluator.EscalateWith(ctx, logger, cl, ev, budget)
	return dec, key
}

// awaitApproval an unanswered request denies (OD-E9-1): never a silent allow
// in enforce mode, and never the provider's own approval prompt, which would
// ask the developer to approve their own filed request.
func (g EnforceGate) awaitApproval(ctx context.Context, logger *log.Logger, t EnforceTarget, cl Governor, clientErr error, dec decision.Decision, key client.ApprovalKey, enforceStart time.Time) decision.Decision {
	if !key.Valid() {
		return ApprovalUndecided(dec, "- this call cannot be tied to an approval record")
	}
	RecordPendingApproval(logger, key, t.ToolName())

	if clientErr != nil {
		logger.Printf("approval hold skipped (client init): %v", clientErr)
		return ApprovalUndecided(dec, "within this hook's budget")
	}

	answered, ok := g.Evaluator.AwaitApproval(ctx, logger, cl, key, enforceStart)
	if !ok {
		return ApprovalUndecided(dec, "within this hook's budget")
	}
	ClaimPendingApproval(key)
	answered.RedactedContent = dec.RedactedContent
	answered.RedactionCategories = dec.RedactionCategories
	return answered
}

// SessionHaltReplay renders a latched run's answer to one gated call: the
// same steps ReplaySessionHalt already takes (no server round-trip; the
// latch is the decided state), reporting back both the decision and the
// ApplyResult instead of recording them itself -- ReplaySessionHalt's own
// callers each have a DIFFERENT way to record a decision (EnforceGate's own
// Record callback here; an adapter's RecordEnforcement call directly, from
// its own pre-gate check and its EnforceGate.OnHalted closure), so this
// shares the rendering steps without forcing one recording contract on
// every caller. localRedaction/toolInput are passed through rather than
// fixed, so a caller with the real values (this gate's own fallback below)
// is not made to render less accurately than one that has none to give
// (an adapter's halt replay, which predates any tool input existing to
// reconstruct).
func SessionHaltReplay(logger *log.Logger, stdout io.Writer, info SessionHaltInfo, localRedaction bool, toolInput json.RawMessage, toolName string, c OutputContract) (decision.Decision, ApplyResult) {
	dec := SessionHaltDecision(info)
	LogEnforceDecision(logger, toolName, dec, ResolveFailurePolicy())
	res := ApplyDecision(stdout, dec, localRedaction, toolInput, c)
	return dec, res
}
