package hookflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// networkFault is what Emit returns when core never answered at all: a
// delivery failure whose FailureClass is "network".
func networkFault() error {
	return fmt.Errorf("%w: dial tcp: connection refused", client.ErrDelivery)
}

// drainOne appends one event for sessionID and drains it once with fn.
func drainOne(t *testing.T, e *Engine, sessionID string, fn emitterFunc, opts DrainOptions) int {
	t.Helper()
	ev := client.DevEvent{EventID: "e-" + sessionID, EventType: client.EventToolCall, SessionID: sessionID}
	if err := e.Spool.Append(ev); err != nil {
		t.Fatalf("append: %v", err)
	}
	n, err := e.DrainSession(context.Background(), sessionID, fn, opts)
	if err != nil && !errors.Is(err, errPassCutShort) {
		t.Fatalf("DrainSession: %v", err)
	}
	return n
}

// TestDrain_RetriesOnceAndDeliversAfterATransientFault: a transient fault on
// the first attempt is retried once; the retry's success delivers the event
// and nothing is ledgered or latched.
func TestDrain_RetriesOnceAndDeliversAfterATransientFault(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	e := NewEngine(t.TempDir())

	attempts := 0
	n := drainOne(t, e, "sess-retry-ok", func(context.Context, client.DevEvent) error {
		attempts++
		if attempts == 1 {
			return networkFault()
		}
		return nil
	}, DrainOptions{Mode: Block, AttemptTimeout: time.Second})

	if attempts != 2 || n != 1 {
		t.Errorf("attempts = %d, delivered = %d; want 2 attempts and 1 delivered", attempts, n)
	}
	if _, halted := SessionHalted("sess-retry-ok"); halted {
		t.Error("a fault the retry recovered from must not halt the run")
	}
	if got := e.Spool.DiscardedCount(); got != 0 {
		t.Errorf("DiscardedCount = %d, want 0", got)
	}
}

// TestDrain_RecordsAFindingWhenTheOneRetryAlsoFails: exactly one retry, then
// an ordinary finding (classified by the retry's own failure class) --
// never a run latch.
func TestDrain_RecordsAFindingWhenTheOneRetryAlsoFails(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	e := NewEngine(t.TempDir())

	attempts := 0
	drainOne(t, e, "sess-retry-fail", func(ctx context.Context, _ client.DevEvent) error {
		attempts++
		if attempts == 1 {
			return networkFault()
		}
		<-ctx.Done()
		return fmt.Errorf("%w: %w", client.ErrDelivery, ctx.Err())
	}, DrainOptions{Mode: Block, AttemptTimeout: 50 * time.Millisecond})

	if attempts != 2 {
		t.Errorf("attempts = %d, want exactly 2 (one attempt, one retry)", attempts)
	}
	if _, halted := SessionHalted("sess-retry-fail"); halted {
		t.Error("a failed retry must never latch the run")
	}
	if got := e.Spool.DiscardedCount(); got != 1 {
		t.Errorf("DiscardedCount = %d, want 1", got)
	}
}

// TestDrain_NeverRetriesANonTransientFailure: a failure that is not a
// transient delivery fault (here, one that is not an ErrDelivery at all,
// the shape a refused or unbuildable event takes) is recorded as a finding
// on the first attempt, without a retry, and never latches the run.
func TestDrain_NeverRetriesANonTransientFailure(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	e := NewEngine(t.TempDir())

	attempts := 0
	drainOne(t, e, "sess-no-retry", func(context.Context, client.DevEvent) error {
		attempts++
		return fmt.Errorf("%w: bad payload", client.ErrUnbuildable)
	}, DrainOptions{Mode: Block, AttemptTimeout: time.Second})

	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (no retry for a non-transient failure)", attempts)
	}
	if _, halted := SessionHalted("sess-no-retry"); halted {
		t.Error("a non-transient failure must never latch the run")
	}
}

// TestDrain_LeavesTheEventQueuedWhenThePassCannotFitTheRetry: a pass whose
// remaining budget cannot cover a full retry leaves the event queued for the
// next pass instead of halting on a failure it never got to retry.
func TestDrain_LeavesTheEventQueuedWhenThePassCannotFitTheRetry(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	e := NewEngine(t.TempDir())
	const sessionID = "sess-no-room"

	ev := client.DevEvent{EventID: "e-" + sessionID, EventType: client.EventToolCall, SessionID: sessionID}
	if err := e.Spool.Append(ev); err != nil {
		t.Fatalf("append: %v", err)
	}
	const attemptTimeout = 200 * time.Millisecond
	attempts := 0
	slowFault := emitterFunc(func(context.Context, client.DevEvent) error {
		attempts++
		time.Sleep(150 * time.Millisecond) // leaves less than one attempt of the pass
		return networkFault()
	})
	ctx, cancel := context.WithTimeout(context.Background(), attemptTimeout+100*time.Millisecond)
	defer cancel()
	_, _ = e.DrainSession(ctx, sessionID, slowFault, DrainOptions{Mode: Block, AttemptTimeout: attemptTimeout})

	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (the retry did not fit this pass)", attempts)
	}
	if _, halted := SessionHalted(sessionID); halted {
		t.Error("an event whose retry did not fit must not halt the run")
	}
	if got := e.Spool.PendingCount(sessionID); got != 1 {
		t.Errorf("PendingCount = %d, want 1 (left queued for the next pass)", got)
	}
}

// TestEscalation_TransientFailureRequeuesInsteadOfHalting: the gate's own
// escalation POST failing transiently never latches the run; like an
// unanswered escalation, its recorded copy goes back to the drainers, whose
// attempt and one retry decide. A proven, non-transient refusal is settled
// and recorded (RecordDeliveryFailure) but never latches either; only a
// real HALT verdict from core still does.
func TestEscalation_TransientFailureRequeuesInsteadOfHalting(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	var got EscalationOutcome = -1
	ev := client.DevEvent{EventID: "e1", EventType: client.EventToolCall, SessionID: "sess-escalate-5xx"}
	ev2 := Evaluator{OnOutcome: func(o EscalationOutcome) { got = o }}
	ev2.runWith(context.Background(), nopLogger(), ev, retryGovernor{err: networkFault()})

	if got != EscalationUnanswered {
		t.Errorf("outcome = %v, want EscalationUnanswered (requeue for the drainers)", got)
	}
	if _, halted := SessionHalted("sess-escalate-5xx"); halted {
		t.Error("a transient escalation failure must not latch the run by itself")
	}

	// 401 and 429 are also treated as Unanswered (neither is a proven,
	// event-specific refusal); an ErrUnbuildable event stands in for a
	// proven 4xx refusal, which IS EscalationSettled.
	got = -1
	ev.SessionID = "sess-escalate-refused"
	ev2.runWith(context.Background(), nopLogger(), ev, retryGovernor{err: fmt.Errorf("%w: bad payload", client.ErrUnbuildable)})
	if got != EscalationSettled {
		t.Errorf("refused outcome = %v, want EscalationSettled", got)
	}
	if _, halted := SessionHalted("sess-escalate-refused"); halted {
		t.Error("a non-transient escalation failure must never latch the run")
	}
}

type retryGovernor struct{ err error }

func (g retryGovernor) Emit(context.Context, client.DevEvent) (client.Evaluation, error) {
	return client.Evaluation{}, g.err
}

func (g retryGovernor) PollApproval(context.Context, client.ApprovalKey) (client.ApprovalStatus, error) {
	return client.ApprovalStatus{}, nil
}

// TestGate_TransientEscalationFailureRequeuesTheObserveCopy: through the whole
// gate, a transient escalation failure still denies the call (fail-closed)
// but does not latch the run; the call's observe copy goes to the head of the
// session's queue, where a drainer's attempt and one retry decide.
func TestGate_TransientEscalationFailureRequeuesTheObserveCopy(t *testing.T) {
	isolateConfig(t)
	t.Setenv(devconfig.EnvApprovalHold, "50")
	t.Setenv(devconfig.EnvEnforcementFile, t.TempDir()+"/enforcements.jsonl")
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	isolateMarkers(t)
	defer devconfig.Pin()()

	var head, tail bool
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 4 * time.Second,
			NewClient: func(*log.Logger) (Governor, error) {
				return retryGovernor{err: networkFault()}, nil
			},
		},
		Record:           func(decision.Decision, ApplyResult) {},
		SpoolObserve:     func() { tail = true },
		SpoolObserveHead: func() { head = true },
	}
	res := gate.Run(context.Background(), discard(), &out, shellTarget{})

	if res.Decision != DecisionDeny {
		t.Errorf("decision = %v, want deny: a failed escalation is still fail-closed", res.Decision)
	}
	if !head || tail {
		t.Errorf("observe copy: head=%v tail=%v, want it requeued at the head only", head, tail)
	}
	if _, halted := SessionHalted("sess-1"); halted {
		t.Error("a transient escalation failure must not latch the run by itself")
	}
}
