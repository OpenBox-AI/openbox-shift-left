package hookflow

import (
	"context"
	"errors"
	"log"
	"sync/atomic"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

const (
	SourceEvaluate         = "evaluate"
	SourceEvaluateFailOpen = "evaluate:fail-open"
)

// DefaultEvaluationTimeout is the budget for the gate's own escalation POST:
// the one, single-attempt /evaluate round-trip a gated call makes once its
// own drain step (if any) is done. Sized so a core that is slow but still
// answering yields a real verdict under always-fail-closed, rather than a
// fail-closed deny after too short a wait.
//
// This is the dominant term in the gate's worst-case wall clock against a
// hung core: uncontended, one gated call pays at most
// GateDrainAttemptTimeout (one doomed drain attempt, if a backlog is queued)
// + DefaultEvaluationTimeout (its own escalation) = 3.5s + 10s = 13.5s; add
// MaxStripeWait (5s) for the contended case where another drainer already
// holds the session's stripe, for 18.5s. Both must stay well inside
// EnforceBudget (29s for a 30s gated-hook ceiling) so the drain step keeps
// real slack rather than being squeezed to zero by this alone.
const DefaultEvaluationTimeout = 10 * time.Second

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
	// MaxTimeout is a TEST SEAM, not a configuration knob: it clamps the
	// configured per-evaluation budget (Budget) so a test can run in
	// milliseconds instead of DefaultEvaluationTimeout's real 10s. Zero (its
	// default) means no clamp. Every production adapter leaves this zero.
	MaxTimeout time.Duration
	// NewClient builds the control-plane transport for the evaluation and for the
	// approval hold that can follow it.
	NewClient func(*log.Logger) (Governor, error)

	// OnOutcome, when set, is called at most once per Escalate call with what
	// happened to the escalation POST (EscalationOutcome), so a caller
	// holding a local copy of the same event (the gate's own observe copy)
	// knows what, if anything, to do with it.
	OnOutcome func(EscalationOutcome)
}

// EscalationOutcome classifies what one Escalate call did with its own
// escalation POST. It never distinguishes "delivered" from "explicitly
// refused and recorded": both mean the caller's own local copy of the event
// needs nothing further, since either core has it or RecordDeliveryFailure
// already recorded that it does not (never latching the run either way).
type EscalationOutcome int32

const (
	// EscalationSettled: core answered, or an explicit non-acceptance was
	// already recorded by RecordDeliveryFailure. Nothing further to spool.
	EscalationSettled EscalationOutcome = iota
	// EscalationNotAttempted: no POST was ever sent (no client configured, a
	// client failed to build, or no budget was left to even try). The
	// caller's own local copy is this event's first delivery attempt.
	EscalationNotAttempted
	// EscalationUnanswered: sent, but this call's own budget ran out before
	// an answer arrived. Never a failure, never latched: the caller's own
	// local copy is queued ahead of the tail for a drainer with a real,
	// unbounded attempt (SpoolObserveHead); core dedupes the eventual
	// re-send on the idempotency key.
	EscalationUnanswered
)

func (t Evaluator) reportOutcome(o EscalationOutcome) {
	if t.OnOutcome != nil {
		t.OnOutcome(o)
	}
}

// escalationReport is a first-wins guard around one Escalate call's outcome:
// run's own goroutine (an explicit failure, already recorded by
// RecordDeliveryFailure) and Escalate's own cctx.Done() branch can each try
// to report, and run's own report can reach here strictly before its result
// ever reaches the channel Escalate selects on -- a scheduling delay between
// "run finished and reported" and "the send Escalate is waiting on actually
// happens" is enough, with no coincidence required. Whichever call lands
// first is the one the caller (the gate's own re-spool dispatch) ever sees;
// a later call is silently dropped rather than allowed to overwrite it,
// which would otherwise let a stale Unanswered re-queue an event whose one
// attempt already failed and was recorded (a second attempt, in spirit, at
// the caller's own local copy of it).
type escalationReport struct {
	done atomic.Bool
	fn   func(EscalationOutcome)
}

func newEscalationReport(fn func(EscalationOutcome)) *escalationReport {
	return &escalationReport{fn: fn}
}

func (r *escalationReport) report(o EscalationOutcome) {
	if r.fn == nil {
		return
	}
	if r.done.CompareAndSwap(false, true) {
		r.fn(o)
	}
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

// errNoTransport is Evaluator.buildClient's own "nothing configured" case:
// no Evaluator.NewClient function was ever set, as opposed to one that was
// set and failed. logTransportFailure tells the two apart in what it logs;
// every caller of buildClient treats them identically otherwise (an
// unattempted escalation, a skipped drain, an approval hold that reports
// undecided at once).
var errNoTransport = errors.New("no control-plane transport configured")

// buildClient is the one "get a client, or the reason there isn't one" step
// every entry point into an evaluation shares: Escalate and run (each still
// building their own, single-step client) and a gate run's own ONE shared
// client (EnforceGate's Run, built once for its drain, escalation and
// approval-hold steps together).
func (t Evaluator) buildClient(logger *log.Logger) (Governor, error) {
	if t.NewClient == nil {
		return nil, errNoTransport
	}
	return t.NewClient(logger)
}

// logTransportFailure logs why a client for this evaluation is not
// available, in the exact wording Escalate/run have always used: the
// "nothing configured" case that never even tried to dial, and a caller
// that reached the control plane's own transport constructor and got an
// error back from it.
func logTransportFailure(logger *log.Logger, err error) {
	if errors.Is(err, errNoTransport) {
		logger.Print("evaluation degrading: no control-plane transport configured")
		return
	}
	logger.Printf("inline evaluation degrading (client init): %v", err)
}

// Escalate runs one bounded evaluation for an already-mapped event, building
// its own client via t.NewClient.
//
// budget<=0 never even builds a client: this call's own escalation was
// unattempted from the start, the same as a NewClient failure, so it reports
// EscalationNotAttempted synchronously rather than racing a goroutine that
// would immediately see its own ctx already expired and misreport
// EscalationUnanswered.
func (t Evaluator) Escalate(ctx context.Context, logger *log.Logger, ev client.DevEvent, budget time.Duration) decision.Decision {
	if budget <= 0 {
		logger.Print("inline evaluation degrading: no evaluation budget remaining")
		t.reportOutcome(EscalationNotAttempted)
		return EvaluationFailOpen("no evaluation budget remaining")
	}
	cl, err := t.buildClient(logger)
	if err != nil {
		logTransportFailure(logger, err)
		t.reportOutcome(EscalationNotAttempted)
		return EvaluationFailOpen("control plane unreachable")
	}
	return t.EscalateWith(ctx, logger, cl, ev, budget)
}

// EscalateWith is Escalate with a client the caller already built and shares
// across its own drain, escalation and approval hold: the identical
// bounded, single-attempt evaluation and outcome reporting, minus the
// client build Escalate does on its own behalf.
func (t Evaluator) EscalateWith(ctx context.Context, logger *log.Logger, cl Governor, ev client.DevEvent, budget time.Duration) decision.Decision {
	if budget <= 0 {
		logger.Print("inline evaluation degrading: no evaluation budget remaining")
		t.reportOutcome(EscalationNotAttempted)
		return EvaluationFailOpen("no evaluation budget remaining")
	}

	// Guarded so run's own goroutine and the cctx.Done() branch below can
	// never both land: run's own report can reach the guard strictly before
	// its result value ever reaches resultCh (a plain scheduling delay
	// between "run finished and reported" and "the send below actually
	// runs" is enough), and without this, a stale Unanswered report reaching
	// the guard after an explicit failure already reported Settled -- and
	// already recorded via RecordDeliveryFailure -- would wrongly requeue an
	// event whose one attempt already failed.
	rep := newEscalationReport(t.OnOutcome)
	runner := t
	runner.OnOutcome = rep.report

	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	resultCh := make(chan decision.Decision, 1)
	go func() { resultCh <- runner.runWith(cctx, logger, ev, cl) }()

	select {
	case dec := <-resultCh:
		return dec
	case <-cctx.Done():
		logger.Printf("inline evaluation degrading (budget %v exceeded)", budget)
		// Reported here, synchronously, rather than left for run's own
		// goroutine to get to on its own: that goroutine may not finish
		// before the caller (a gate hook) returns and the process exits,
		// and the requeue this outcome triggers (SpoolObserveHead) must not
		// depend on it doing so. The guard above is what keeps this from
		// ever overwriting a report run already made.
		rep.report(EscalationUnanswered)
		return EvaluationFailOpen("evaluation budget exceeded")
	}
}

func (t Evaluator) run(cctx context.Context, logger *log.Logger, ev client.DevEvent) decision.Decision {
	cl, err := t.buildClient(logger)
	if err != nil {
		logTransportFailure(logger, err)
		t.reportOutcome(EscalationNotAttempted)
		return EvaluationFailOpen("control plane unreachable")
	}
	return t.runWith(cctx, logger, ev, cl)
}

// runWith is run's own emit-and-classify step, factored out so EscalateWith
// (a caller that already built its own client) can reuse the exact same
// unanswered-vs-explicit-failure discriminator without building a second
// client for the same escalation.
func (t Evaluator) runWith(cctx context.Context, logger *log.Logger, ev client.DevEvent, cl Governor) decision.Decision {
	if cl == nil {
		t.reportOutcome(EscalationNotAttempted)
		return EvaluationFailOpen("control plane unreachable")
	}
	eval, err := cl.Emit(cctx, ev)
	if err != nil {
		// Read before anything else can change cctx's own error: the same
		// rule DrainOptions.RequeueUnanswered uses (attemptLines) to tell an
		// unanswered attempt from a proven one.
		unanswered := cctx.Err() != nil
		logger.Printf("inline evaluation degrading (emit): %v", err)
		class := client.FailureClass(err)
		// A transient failure (timeout, network, 5xx) is treated like
		// silence: the call is still denied, but nothing is recorded here.
		// A 401 or 429 is treated the same way -- neither is a proven,
		// event-specific refusal (client.ErrRefused's own contract; see
		// client.isRefusal) -- a datastore hiccup or an exhausted rate limit
		// must not be misreported as this event's own fault. Either way, the
		// gated call's own recorded copy goes back to the drainers, whose
		// attempt and one retry decide what happens to it.
		if unanswered || client.RetryableDelivery(err) || class == "401" || class == "429" {
			t.reportOutcome(EscalationUnanswered)
			return EvaluationFailOpen("evaluation undelivered")
		}
		// A proven non-acceptance (ErrRefused's other 4xx classes, or an
		// unbuildable event): recorded as a delivery-failure finding, but
		// never latches the run -- a delivery failure denies only this
		// call; the run itself continues.
		RecordDeliveryFailure(logger, ev, err)
		t.reportOutcome(EscalationSettled)
		return EvaluationFailOpen("evaluation undelivered")
	}
	t.reportOutcome(EscalationSettled)
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
