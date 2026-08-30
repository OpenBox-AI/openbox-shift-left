package hookflow

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

type shellTarget struct{}

func (shellTarget) SessionID() string          { return "sess-1" }
func (shellTarget) ToolName() string           { return "Bash" }
func (shellTarget) ToolInput() json.RawMessage { return json.RawMessage(`{"command":"rm -rf /tmp/x"}`) }
func (shellTarget) HighRisk() bool             { return true }
func (shellTarget) DecisionRequest(bool) decision.DecisionRequest {
	return decision.DecisionRequest{SessionID: "sess-1", Tool: client.Tool{Name: "Bash", Kind: client.ToolShell}}
}
func (shellTarget) DevEvent(*client.Content) (client.DevEvent, bool) {
	return client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       "evt-1",
		EventType:     client.EventToolCall,
		SessionID:     "sess-1",
		DeveloperDID:  "did:aip:dev",
		Timestamp:     "2026-08-01T12:00:00Z",
		Tool:          client.Tool{Name: "Bash", Kind: client.ToolShell},
	}, true
}

type approvalGovernor struct{ *fakeGovernor }

func (g approvalGovernor) Emit(context.Context, client.DevEvent) (client.Evaluation, error) {
	return client.Evaluation{
		Verdict:           client.VerdictRequireApproval,
		Reason:            "production shell command",
		GovernanceEventID: "ge-1",
	}, nil
}

func runGate(t *testing.T, g *fakeGovernor, holdMS string) (string, decision.Decision) {
	t.Helper()
	isolateConfig(t)
	t.Setenv(devconfig.EnvTier2, "1")
	t.Setenv(devconfig.EnvApprovalHold, holdMS)
	t.Setenv(devconfig.EnvEnforcementFile, t.TempDir()+"/enforcements.jsonl")
	isolateMarkers(t)
	defer devconfig.Pin()()

	var recorded decision.Decision
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 4 * time.Second,
			NewClient:  func(*log.Logger) (Governor, error) { return approvalGovernor{fakeGovernor: g}, nil },
		},
		Record: func(dec decision.Decision, _ ApplyResult) { recorded = dec },
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})
	return out.String(), recorded
}

// TestGate_ApprovedDuringTheHoldProceeds the happy path of the whole design:
// the request is filed, an approver answers inside the hold, and the developer
// sees nothing; the tool call proceeds.
func TestGate_ApprovedDuringTheHoldProceeds(t *testing.T) {
	expiry := time.Now().Add(30 * time.Minute)
	g := &fakeGovernor{replies: []func() (client.ApprovalStatus, error){
		pending(expiry), decided(client.VerdictAllow, expiry),
	}}
	out, rec := runGate(t, g, "5000")
	if out != "" {
		t.Errorf("an approved call must write nothing to the provider, got %q", out)
	}
	if rec.Evaluation.Verdict != client.VerdictAllow {
		t.Errorf("recorded verdict = %q, want ALLOW", rec.Evaluation.Verdict)
	}
	if rec.Source != SourceApprovalDecided {
		t.Errorf("source = %q, want the decision to be attributed to the approver", rec.Source)
	}
}

func TestGate_RejectedDuringTheHoldDenies(t *testing.T) {
	expiry := time.Now().Add(30 * time.Minute)
	g := &fakeGovernor{replies: []func() (client.ApprovalStatus, error){decided(client.VerdictHalt, expiry)}}
	out, rec := runGate(t, g, "5000")
	if out != DecisionDeny+"\n" {
		t.Errorf("provider output = %q, want a deny", out)
	}
	if rec.Evaluation.Verdict != client.VerdictHalt {
		t.Errorf("recorded verdict = %q, want HALT", rec.Evaluation.Verdict)
	}
}

// TestGate_UndecidedApprovalDenies oD-E9-1: budget exhausted with the request
// still undecided denies; never a silent allow, and never the provider's self-
// approval prompt.
func TestGate_UndecidedApprovalDenies(t *testing.T) {
	g := &fakeGovernor{replies: []func() (client.ApprovalStatus, error){pending(time.Now().Add(30 * time.Minute))}}
	out, rec := runGate(t, g, "600")
	if out != DecisionDeny+"\n" {
		t.Errorf("provider output = %q, want a deny (ask would be self-approval)", out)
	}
	if rec.Evaluation.Verdict != client.VerdictHalt {
		t.Errorf("recorded verdict = %q, want HALT", rec.Evaluation.Verdict)
	}
	if !strings.Contains(rec.Evaluation.Reason, "ge-1") {
		t.Errorf("deny reason %q must name the approval reference so the model can say what it is waiting on", rec.Evaluation.Reason)
	}
	if g.polls == 0 {
		t.Error("the gate never polled — the request was filed but nobody waited for it")
	}
}

// TestGate_MarkerHandoffToTheWatcher the handoff between the two tiers: an
// exhausted hold leaves the marker standing so the background watcher owns the
// tail, while a hold that answered takes it away so nobody announces an
// outcome the call already saw.
func TestGate_MarkerHandoffToTheWatcher(t *testing.T) {
	expiry := time.Now().Add(30 * time.Minute)
	key := client.ApprovalKeyFor(mustDevEvent(t))

	t.Run("exhausted hold leaves it for the watcher", func(t *testing.T) {
		g := &fakeGovernor{replies: []func() (client.ApprovalStatus, error){pending(expiry)}}
		runGate(t, g, "600")
		if _, err := os.Stat(PendingApprovalPath(key)); err != nil {
			t.Errorf("no marker after an exhausted hold: %v — the late decision would land unannounced", err)
		}
	})

	t.Run("answered hold takes it away", func(t *testing.T) {
		g := &fakeGovernor{replies: []func() (client.ApprovalStatus, error){decided(client.VerdictAllow, expiry)}}
		runGate(t, g, "5000")
		if _, err := os.Stat(PendingApprovalPath(key)); err == nil {
			t.Error("marker survived a hold that answered — the watcher would repeat the outcome")
		}
	})
}

func mustDevEvent(t *testing.T) client.DevEvent {
	t.Helper()
	ev, ok := shellTarget{}.DevEvent(nil)
	if !ok {
		t.Fatal("shellTarget must map")
	}
	return ev
}

type degradedGovernor struct{ *fakeGovernor }

func (g degradedGovernor) Emit(context.Context, client.DevEvent) (client.Evaluation, error) {
	return client.Evaluation{}, client.ErrDelivery
}

type countingGovernor struct {
	*fakeGovernor
	emits *int32
}

func (g countingGovernor) Emit(context.Context, client.DevEvent) (client.Evaluation, error) {
	atomic.AddInt32(g.emits, 1)
	return client.Evaluation{}, client.ErrDelivery
}

// TestGate_DoesNotRetryAFailedEvaluation the gate must not retry a failed
// evaluation. Stated at this layer deliberately: the transport has its own
// bounded retry, which is a different decision made in a different place.
func TestGate_DoesNotRetryAFailedEvaluation(t *testing.T) {
	isolateConfig(t)
	isolateMarkers(t)
	t.Setenv(devconfig.EnvEnforcementFile, t.TempDir()+"/enforcements.jsonl")
	defer devconfig.Pin()()

	var emits int32
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 4 * time.Second,
			NewClient: func(*log.Logger) (Governor, error) {
				return countingGovernor{fakeGovernor: &fakeGovernor{}, emits: &emits}, nil
			},
		},
		Record: func(decision.Decision, ApplyResult) {},
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})

	if n := atomic.LoadInt32(&emits); n != 1 {
		t.Errorf("gate asked for a verdict %d times, want exactly 1 — a retry inside the "+
			"gate amplifies a core outage across every tool call of every session", n)
	}
}
