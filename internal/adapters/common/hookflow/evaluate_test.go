package hookflow

import (
	"context"
	"encoding/json"
	"log"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

type testContract struct{ approval string }

func (c testContract) ApprovalDecision() string { return c.approval }
func (testContract) ContentFieldKeys() []string { return nil }
func (testContract) Render(d, reason string, updated json.RawMessage) ([]byte, string) {
	if d == "" {
		return nil, ""
	}
	return []byte(d), d
}

func verdictDecision(v client.Verdict) decision.Decision {
	return decision.Decision{Evaluation: client.Evaluation{Verdict: v}}
}

// TestShouldEscalate is deleted with ShouldEscalate.

// TestKeepTighterHoldsTheTier1Floor is deleted with KeepTighter. It covered
// the one thing that function existed for: a degraded evaluation must not
// replace a local deny/ask with VerdictUnknown and let the call through;
// enforcement loosening itself on an outage.

// explicitFailGovernor answers an explicit, non-transient failure
// immediately (no delay), so run's own unanswered check reads cctx as still
// alive: the scenario this test needs is an explicit failure that
// genuinely happened before the budget ran out, not a slow one. It is
// non-transient on purpose: a transient one (timeout, network, 5xx) is
// requeued rather than latched (TestEscalation_TransientFailureRequeuesInsteadOfHalting).
type explicitFailGovernor struct{}

func (explicitFailGovernor) Emit(context.Context, client.DevEvent) (client.Evaluation, error) {
	return client.Evaluation{}, errNonTransient
}
func (explicitFailGovernor) PollApproval(context.Context, client.ApprovalKey) (client.ApprovalStatus, error) {
	return client.ApprovalStatus{}, nil
}

// TestEscalate_ExplicitFailureOutcomeSurvivesALateUnansweredReport pins the
// first-wins guarantee an escalation's outcome report needs: run's own
// goroutine can report an explicit failure (already recorded through
// RecordDeliveryFailure, never latched) strictly before Escalate's own
// timeout branch gets a chance to report Unanswered for the very same
// call -- run reports before its result ever
// reaches the channel Escalate selects on, so a naive last-write callback
// would let the later, stale Unanswered overwrite the correct Settled and
// wrongly requeue an event whose one attempt already failed and was
// recorded.
//
// Deterministic: no timing race. run() is called directly with a long-lived
// ctx (so its own unanswered check reads false, exactly as it would for a
// genuine fast explicit failure), and Escalate's own cctx.Done() branch is
// simulated by calling the SAME guarded reporter a second time immediately
// after -- the exact sequence the real race can produce, without depending
// on the goroutine scheduler to reproduce it.
func TestEscalate_ExplicitFailureOutcomeSurvivesALateUnansweredReport(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	const unset = EscalationOutcome(-1)
	got := unset
	rep := newEscalationReport(func(o EscalationOutcome) { got = o })

	ev := devEventFor("run-first-wins", client.EventToolCall)
	evaluator := Evaluator{
		NewClient: func(*log.Logger) (Governor, error) { return explicitFailGovernor{}, nil },
		OnOutcome: rep.report,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour) // nowhere near exhausted
	defer cancel()
	evaluator.run(ctx, nopLogger(), ev)

	// Simulates Escalate's own cctx.Done() branch observing the deadline
	// AFTER run() already decided and reported -- the exact race this guard
	// closes.
	rep.report(EscalationUnanswered)

	if got != EscalationSettled {
		t.Fatalf("reported outcome = %v, want EscalationSettled: an explicit failure that already reported must win over a later, stale Unanswered report", got)
	}
	if _, halted := SessionHalted("run-first-wins"); halted {
		t.Fatal("the explicit failure must never latch the run")
	}
}

// TestEscalate_ExplicitFailureReportsSettledThroughTheRealPath is the same
// invariant through Escalate's own public entry point (not run() directly,
// unlike the deterministic race reproduction above): a generous budget
// against a fast, explicit failure must report Settled and latch, never
// Unanswered -- the ordinary case the guard must not disturb.
func TestEscalate_ExplicitFailureReportsSettledThroughTheRealPath(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	const unset = EscalationOutcome(-1)
	got := unset
	ev := devEventFor("run-through-escalate", client.EventToolCall)
	evaluator := Evaluator{
		NewClient: func(*log.Logger) (Governor, error) { return explicitFailGovernor{}, nil },
		OnOutcome: func(o EscalationOutcome) { got = o },
	}

	evaluator.Escalate(context.Background(), nopLogger(), ev, time.Hour)

	if got != EscalationSettled {
		t.Fatalf("reported outcome = %v, want EscalationSettled", got)
	}
	if _, halted := SessionHalted("run-through-escalate"); halted {
		t.Fatal("the explicit failure must never latch the run")
	}
}

// TestEscalationReport_FirstCallWinsEitherOrder is the guard's own
// unit-level property, independent of run/Escalate: whichever outcome is
// reported first survives, in either order.
func TestEscalationReport_FirstCallWinsEitherOrder(t *testing.T) {
	t.Run("settled then unanswered", func(t *testing.T) {
		var got EscalationOutcome = -1
		rep := newEscalationReport(func(o EscalationOutcome) { got = o })
		rep.report(EscalationSettled)
		rep.report(EscalationUnanswered)
		if got != EscalationSettled {
			t.Errorf("got %v, want EscalationSettled", got)
		}
	})
	t.Run("unanswered then settled", func(t *testing.T) {
		var got EscalationOutcome = -1
		rep := newEscalationReport(func(o EscalationOutcome) { got = o })
		rep.report(EscalationUnanswered)
		rep.report(EscalationSettled)
		if got != EscalationUnanswered {
			t.Errorf("got %v, want EscalationUnanswered", got)
		}
	})
	t.Run("nil fn is inert", func(t *testing.T) {
		rep := newEscalationReport(nil)
		rep.report(EscalationSettled) // must not panic
	})
}

// TestRunWith_401And429AreUnansweredOtherRefusalsAreSettledAndUnlatched pins
// the exact classification evaluate.go's implementation depends on,
// through a REAL client against a real HTTP status
// (client.FailureClass/isRefusal's own contract, not a fake error): 401 and
// 429 are neither of them a proven, event-specific refusal (core maps a
// datastore hiccup to 401, and 429 is merely exhausted, per
// client.ErrRefused's own doc comment), so both are EscalationUnanswered,
// same as silence -- never recorded, never latched. A proven 4xx refusal
// (400) is EscalationSettled and recorded via RecordDeliveryFailure, but
// never latches the run either; only a real HALT verdict still does.
func TestRunWith_401And429AreUnansweredOtherRefusalsAreSettledAndUnlatched(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		wantOutcome EscalationOutcome
	}{
		{"401 identity or datastore fault", 401, EscalationUnanswered},
		{"429 exhausted, not refused", 429, EscalationUnanswered},
		{"400 a proven refusal", 400, EscalationSettled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(devconfig.EnvHaltDir, t.TempDir())
			fc := fakecore.New(t, fakecore.Script{AlwaysStatus: tc.status})
			zero := 0
			key := fakecore.APIKey()
			cl, err := client.New(client.Config{
				BaseURL:            fc.URL(),
				APIKey:             key,
				WorkloadPrivateKey: fakecore.WorkloadPrivateKey(),
				MaxRetries:         &zero,
			})
			if err != nil {
				t.Fatalf("client.New: %v", err)
			}

			var got EscalationOutcome = -1
			ev := client.DevEvent{
				SchemaVersion: client.SchemaVersion, EventID: "e-" + tc.name, EventType: client.EventToolCall,
				SessionID: "sess-" + tc.name, DeveloperDID: "did:aip:dev", Timestamp: "2026-08-01T12:00:00Z",
			}
			evaluator := Evaluator{OnOutcome: func(o EscalationOutcome) { got = o }}
			evaluator.runWith(context.Background(), nopLogger(), ev, cl)

			if got != tc.wantOutcome {
				t.Errorf("outcome = %v, want %v", got, tc.wantOutcome)
			}
			if _, halted := SessionHalted(ev.SessionID); halted {
				t.Error("no failure class reaching runWith may ever latch the run")
			}
		})
	}
}
