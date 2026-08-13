package hookflow

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
	"github.com/openbox-ai/openbox-shift-left/provider"
)

// fakeGovernor answers polls from a scripted sequence, so a test can express
// "pending, pending, then approved" without any wall-clock coupling.
type fakeGovernor struct {
	mu      sync.Mutex
	replies []func() (client.ApprovalStatus, error)
	polls   int
	gotKey  client.ApprovalKey
}

func (f *fakeGovernor) Emit(context.Context, client.DevEvent) (client.Evaluation, error) {
	return client.Evaluation{}, nil
}

func (f *fakeGovernor) PollApproval(_ context.Context, k client.ApprovalKey) (client.ApprovalStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotKey = k
	i := f.polls
	f.polls++
	if i >= len(f.replies) {
		i = len(f.replies) - 1 // the last reply repeats
	}
	return f.replies[i]()
}

func pending(expiry time.Time) func() (client.ApprovalStatus, error) {
	return func() (client.ApprovalStatus, error) {
		return client.ApprovalStatus{EventID: "ge-1", Verdict: client.VerdictRequireApproval, ExpiresAt: expiry}, nil
	}
}

func decided(v client.Verdict, expiry time.Time) func() (client.ApprovalStatus, error) {
	return func() (client.ApprovalStatus, error) {
		return client.ApprovalStatus{EventID: "ge-1", Verdict: v, Reason: "approver said so", ExpiresAt: expiry}, nil
	}
}

func holdTier2(t *testing.T, g *fakeGovernor, holdMS string) Tier2 {
	t.Helper()
	isolateConfig(t)
	t.Setenv(devconfig.EnvApprovalHold, holdMS)
	return Tier2{
		Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
		MaxTimeout: 4 * time.Second,
		NewClient:  func(*log.Logger) (Governor, error) { return g, nil },
	}
}

func discard() *log.Logger { return log.New(&strings.Builder{}, "", 0) }

func TestAwaitApproval_DecidedDuringTheHold(t *testing.T) {
	expiry := time.Now().Add(30 * time.Minute)
	for _, tc := range []struct {
		name    string
		verdict client.Verdict
	}{
		{"approved", client.VerdictAllow},
		{"rejected", client.VerdictHalt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &fakeGovernor{replies: []func() (client.ApprovalStatus, error){
				pending(expiry), pending(expiry), decided(tc.verdict, expiry),
			}}
			key := client.ApprovalKey{WorkflowID: "w", RunID: "r", ActivityID: "a"}
			dec, ok := holdTier2(t, g, "5000").AwaitApproval(context.Background(), discard(), key, time.Now())
			if !ok {
				t.Fatal("a decision that landed during the hold must be reported")
			}
			if dec.Evaluation.Verdict != tc.verdict {
				t.Errorf("verdict = %q, want %q", dec.Evaluation.Verdict, tc.verdict)
			}
			if dec.Source != SourceApprovalDecided {
				t.Errorf("source = %q, want %q so the audit can tell a decision from an escalation", dec.Source, SourceApprovalDecided)
			}
			if g.gotKey != key {
				t.Errorf("polled key %+v, want %+v", g.gotKey, key)
			}
		})
	}
}

// A transport blip mid-hold is not evidence that the approval was refused: the
// hold keeps polling on its cadence rather than giving up on the first error.
func TestAwaitApproval_SurvivesAPollFailure(t *testing.T) {
	expiry := time.Now().Add(30 * time.Minute)
	g := &fakeGovernor{replies: []func() (client.ApprovalStatus, error){
		func() (client.ApprovalStatus, error) { return client.ApprovalStatus{}, client.ErrApprovalNotFound },
		func() (client.ApprovalStatus, error) { return client.ApprovalStatus{}, errors.New("connection reset") },
		decided(client.VerdictAllow, expiry),
	}}
	dec, ok := holdTier2(t, g, "5000").AwaitApproval(context.Background(), discard(),
		client.ApprovalKey{WorkflowID: "w", RunID: "r", ActivityID: "a"}, time.Now())
	if !ok || dec.Evaluation.Verdict != client.VerdictAllow {
		t.Fatalf("hold gave up on a transient fault: ok=%t dec=%+v", ok, dec)
	}
}

// Once core's window has passed nothing will decide the request, so the hold
// stops rather than spending the rest of its budget.
func TestAwaitApproval_StopsWhenTheWindowCloses(t *testing.T) {
	g := &fakeGovernor{replies: []func() (client.ApprovalStatus, error){pending(time.Now().Add(-time.Minute))}}
	start := time.Now()
	if _, ok := holdTier2(t, g, "5000").AwaitApproval(context.Background(), discard(),
		client.ApprovalKey{WorkflowID: "w", RunID: "r", ActivityID: "a"}, time.Now()); ok {
		t.Fatal("a closed window must not report a decision")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("hold ran %v after the window closed — it should stop immediately", elapsed)
	}
}

func TestAwaitApproval_UndecidedWithinBudget(t *testing.T) {
	g := &fakeGovernor{replies: []func() (client.ApprovalStatus, error){pending(time.Now().Add(30 * time.Minute))}}
	if _, ok := holdTier2(t, g, "700").AwaitApproval(context.Background(), discard(),
		client.ApprovalKey{WorkflowID: "w", RunID: "r", ActivityID: "a"}, time.Now()); ok {
		t.Fatal("an undecided request must not report a decision")
	}
	if g.polls < 2 {
		t.Errorf("hold polled %d times in ~700ms — it should poll on its cadence, not once", g.polls)
	}
}

// The hold is clamped by whatever is left of the provider's hook timeout. With
// none left there is no room to hold, so the caller denies immediately rather
// than overrun the hook and be killed (which fails open).
func TestHoldBudget_ClampedByTheHookCeiling(t *testing.T) {
	tr := Tier2{Ceiling: provider.HookCeiling{Gating: 30 * time.Second}}
	if got := tr.HoldBudget(time.Now(), 20*time.Second); got != 20*time.Second {
		t.Errorf("fresh hold budget = %v, want the configured 20s", got)
	}
	if got := tr.HoldBudget(time.Now().Add(-25*time.Second), 20*time.Second); got > 5*time.Second {
		t.Errorf("hold budget = %v, want it clamped to the ~4s remaining", got)
	}
	if got := tr.HoldBudget(time.Now().Add(-30*time.Second), 20*time.Second); got > 0 {
		t.Errorf("exhausted-ceiling hold budget = %v, want non-positive", got)
	}

	g := &fakeGovernor{replies: []func() (client.ApprovalStatus, error){pending(time.Now().Add(time.Hour))}}
	tr2 := holdTier2(t, g, "20000")
	if _, ok := tr2.AwaitApproval(context.Background(), discard(),
		client.ApprovalKey{WorkflowID: "w", RunID: "r", ActivityID: "a"},
		time.Now().Add(-30*time.Second)); ok {
		t.Error("a hold with no budget must not report a decision")
	}
	if g.polls != 0 {
		t.Errorf("hold with no budget made %d polls, want 0", g.polls)
	}
}

// OD-E9-1: an undecided approval DENIES, and names the reference so the model
// can say what is being waited on. It must never fall through to the provider's
// own prompt, which would ask the developer to approve their own request.
func TestApprovalUndecided_DeniesWithTheReference(t *testing.T) {
	dec := decision.Decision{Evaluation: client.Evaluation{
		Verdict:           client.VerdictRequireApproval,
		GovernanceEventID: "ge-42",
	}}
	out := ApprovalUndecided(dec, "within this hook's budget")
	if out.Evaluation.Verdict != client.VerdictHalt {
		t.Errorf("verdict = %q, want HALT so the apply cascade denies", out.Evaluation.Verdict)
	}
	d, reason := MapVerdict(out.Evaluation, testContract{approval: "ask"})
	if d != DecisionDeny {
		t.Errorf("decision = %q, want deny — never the provider's self-approval prompt", d)
	}
	if !strings.Contains(reason, "ge-42") {
		t.Errorf("deny reason %q must name the approval reference", reason)
	}
}

func TestResolveApprovalHold(t *testing.T) {
	isolateConfig(t)
	if got := resolveApprovalHold(); got != DefaultApprovalHold {
		t.Errorf("unconfigured hold = %v, want the default %v", got, DefaultApprovalHold)
	}
	t.Setenv(devconfig.EnvApprovalHold, "1500")
	if got := resolveApprovalHold(); got != 1500*time.Millisecond {
		t.Errorf("configured hold = %v, want 1.5s", got)
	}
	// A value that would overflow time.Duration on the multiply is clamped.
	t.Setenv(devconfig.EnvApprovalHold, "9223372036854775807")
	if got := resolveApprovalHold(); got != time.Hour {
		t.Errorf("overflowing hold = %v, want the 1h clamp", got)
	}
}
