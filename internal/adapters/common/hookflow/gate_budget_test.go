package hookflow

import (
	"bytes"
	"context"
	"log"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// deadlineRecordingGovernor's Emit records how much time was left on the ctx
// it was actually given -- the one signal that proves which timeout
// (GateDrainAttemptTimeout vs. DefaultEvaluationTimeout) governed this
// particular call, without depending on wall-clock timing races.
type deadlineRecordingGovernor struct {
	*fakeGovernor
	remaining []time.Duration // one per Emit, in call order
}

func (g *deadlineRecordingGovernor) Emit(ctx context.Context, _ client.DevEvent) (client.Evaluation, error) {
	left := time.Duration(-1)
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	g.remaining = append(g.remaining, left)
	return client.Evaluation{Verdict: client.VerdictAllow}, nil
}

// TestGate_DrainAttemptIsBoundedByGateDrainAttemptTimeout pins that a queued
// backlog's drain attempt is bounded by GateDrainAttemptTimeout (3.5s), never
// by DefaultEvaluationTimeout (10s): with a generous ceiling and an unclamped
// MaxTimeout, plenty of slack remains for the drain, so the only thing left
// to tell the two timeouts apart is the ctx deadline the drain attempt itself
// was actually given.
func TestGate_DrainAttemptIsBoundedByGateDrainAttemptTimeout(t *testing.T) {
	gateDrainEnv(t)
	defer devconfig.Pin()()

	engine := NewEngine(t.TempDir())
	seedQueued(t, engine, "sess-1", "seed-1", client.EventToolCall)

	gov := &deadlineRecordingGovernor{fakeGovernor: &fakeGovernor{}}
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 0, // unclamped: production shape
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record: func(decision.Decision, ApplyResult) {},
		Queue:  engine,
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})

	// The drain runs before the escalation, so the first Emit is the queued
	// event's drain attempt and the last is the gate's own escalation.
	if len(gov.remaining) < 2 {
		t.Fatalf("Emit calls = %d, want the drain attempt then the escalation; the test proves nothing", len(gov.remaining))
	}
	drain, escalation := gov.remaining[0], gov.remaining[len(gov.remaining)-1]
	// Generous scheduling slack (1s) while still separating the two
	// candidate values, which are 6.5s apart.
	if drain <= 0 || drain > GateDrainAttemptTimeout+time.Second {
		t.Errorf("drain attempt ctx had %v left, want <= ~%v (GateDrainAttemptTimeout), never DefaultEvaluationTimeout",
			drain, GateDrainAttemptTimeout)
	}
	if escalation < DefaultEvaluationTimeout-time.Second || escalation > DefaultEvaluationTimeout {
		t.Errorf("escalation ctx had %v left, want ~%v (DefaultEvaluationTimeout, unclamped)", escalation, DefaultEvaluationTimeout)
	}
}

// TestEvaluator_BudgetIsTenSecondsUnclamped pins DefaultEvaluationTimeout's
// new value end to end through Evaluator.Budget: an unclamped Evaluator
// (MaxTimeout == 0, production shape) against a ceiling with plenty of
// remaining slack hands out the full 10s, not the old 3.5s.
func TestEvaluator_BudgetIsTenSecondsUnclamped(t *testing.T) {
	ev := Evaluator{Ceiling: provider.HookCeiling{Gating: 1000 * time.Second}}
	got := ev.Budget(time.Now(), DefaultEvaluationTimeout)
	if got < 9*time.Second || got > DefaultEvaluationTimeout {
		t.Errorf("Budget = %v, want ~%v (DefaultEvaluationTimeout, unclamped)", got, DefaultEvaluationTimeout)
	}
}

// slowThenAnswersGovernor answers ALLOW after a fixed, real delay -- standing
// in for a core that is slow but still alive, the exact case
// DefaultEvaluationTimeout's 10s budget exists to tolerate.
type slowThenAnswersGovernor struct {
	*fakeGovernor
	delay time.Duration
}

func (g slowThenAnswersGovernor) Emit(ctx context.Context, _ client.DevEvent) (client.Evaluation, error) {
	select {
	case <-time.After(g.delay):
		return client.Evaluation{Verdict: client.VerdictAllow}, nil
	case <-ctx.Done():
		return client.Evaluation{}, ctx.Err()
	}
}

// TestGate_SlowButAnsweringCoreGetsARealVerdictNow proves the actual
// behavior change: a core that answers after 4s -- well past the OLD 3.5s
// budget, comfortably inside the NEW 10s one -- now yields a real,
// SourceEvaluate verdict instead of a manufactured fail-open. Uses a real 4s
// delay (unavoidable: this is exactly the wall-clock behavior being proven),
// kept to the minimum needed to cross the old boundary.
func TestGate_SlowButAnsweringCoreGetsARealVerdictNow(t *testing.T) {
	gateDrainEnv(t)
	defer devconfig.Pin()()

	gov := slowThenAnswersGovernor{fakeGovernor: &fakeGovernor{}, delay: 4 * time.Second}
	var recorded decision.Decision
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 0, // unclamped: production shape
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record: func(dec decision.Decision, _ ApplyResult) { recorded = dec },
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})

	if recorded.Source != SourceEvaluate {
		t.Errorf("source = %q, want %q: a core answering after 4s must yield a real verdict under the new 10s budget", recorded.Source, SourceEvaluate)
	}
	if recorded.Evaluation.Verdict != client.VerdictAllow {
		t.Errorf("verdict = %q, want ALLOW", recorded.Evaluation.Verdict)
	}
	if recorded.FailOpen {
		t.Error("a real, delivered verdict must not be marked FailOpen")
	}
}

// TestBudgetConstants_StayWithinTheDocumentedBounds pins the arithmetic
// DefaultEvaluationTimeout's and GateDrainAttemptTimeout's own doc comments
// both reason about: the drain attempt must never be the longer of the two,
// and the worst-case contended wall clock against a hung core
// (GateDrainAttemptTimeout + DefaultEvaluationTimeout + MaxStripeWait) must
// stay well inside EnforceBudget for a 30s-ceiling gated hook.
func TestBudgetConstants_StayWithinTheDocumentedBounds(t *testing.T) {
	if GateDrainAttemptTimeout > DefaultEvaluationTimeout {
		t.Errorf("GateDrainAttemptTimeout (%v) must never exceed DefaultEvaluationTimeout (%v)",
			GateDrainAttemptTimeout, DefaultEvaluationTimeout)
	}
	worstCaseContended := GateDrainAttemptTimeout + DefaultEvaluationTimeout + MaxStripeWait
	budget := EnforceBudget(provider.HookCeiling{Gating: 30 * time.Second})
	if worstCaseContended >= budget {
		t.Errorf("worst-case contended wall clock %v must stay well inside EnforceBudget(30s) = %v, "+
			"or the drain step is squeezed to zero slack", worstCaseContended, budget)
	}
}
