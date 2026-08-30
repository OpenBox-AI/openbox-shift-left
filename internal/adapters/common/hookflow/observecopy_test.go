package hookflow

import (
	"bytes"
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// Core does not dedupe developer events on their id, so whichever of the two
// runs, the other must not; otherwise one tool call becomes two stored rows
// and two Merkle leaves.

func deliveringGate(t *testing.T, gov Governor, tier2Enabled string) (spooled bool) {
	t.Helper()
	isolateConfig(t)
	t.Setenv(devconfig.EnvTier2, tier2Enabled)
	t.Setenv(devconfig.EnvApprovalHold, "50")
	t.Setenv(devconfig.EnvEnforcementFile, t.TempDir()+"/enforcements.jsonl")
	isolateMarkers(t)
	defer devconfig.Pin()()

	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 4 * time.Second,
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record:       func(decision.Decision, ApplyResult) {},
		SpoolObserve: func() { spooled = true },
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})
	return spooled
}

// TestGate_ObserveCopySkippedWhenEscalationDelivered the bug, in one
// assertion.
func TestGate_ObserveCopySkippedWhenEscalationDelivered(t *testing.T) {
	gov := &fakeGovernor{}
	if deliveringGate(t, gov, "1") {
		t.Error("escalation delivered this event; spooling it again stores a second " +
			"ActivityStarted for one tool call (core does not dedupe)")
	}
}

// TestGate_ObserveCopySpooledWhenEscalationUndelivered the fail-open safety
// net, and the reason suppression must key on the transport rather than on
// "did we try": nothing reached core, so the spool copy is the only remaining
// path for this telemetry and must survive.
func TestGate_ObserveCopySpooledWhenEscalationUndelivered(t *testing.T) {
	gov := degradedGovernor{fakeGovernor: &fakeGovernor{}}
	if !deliveringGate(t, gov, "1") {
		t.Error("escalation never delivered; dropping the observe copy loses the event entirely")
	}
}

// TestGate_ObserveCopySkippedWhenApprovalWasFiled a REQUIRE_APPROVAL verdict
// is delivered like any other; it is a filed record, which is precisely a
// stored row. The hold that follows must not un-suppress the observe copy.
func TestGate_ObserveCopySkippedWhenApprovalWasFiled(t *testing.T) {
	gov := approvalGovernor{fakeGovernor: &fakeGovernor{
		replies: []func() (client.ApprovalStatus, error){pending(time.Now().Add(time.Hour))},
	}}
	if deliveringGate(t, gov, "1") {
		t.Error("the filed approval IS the stored event; the observe copy duplicates it")
	}
}

// TestGate_DeprecatedTier2ToggleNoLongerSuppressesEvaluation the deprecated
// tier2 toggle no longer suppresses evaluation : it is still parsed for back-
// compat but must not change behaviour.
func TestGate_DeprecatedTier2ToggleNoLongerSuppressesEvaluation(t *testing.T) {
	gov := &fakeGovernor{}
	if deliveringGate(t, gov, "0") {
		t.Error("tier2=0 suppressed the escalation; the key is parsed-but-ignored now, " +
			"and an org that set it once must not stay ungoverned")
	}
}

// slowGovernor's Emit outlives the escalation budget and only then reports
// success.
type slowGovernor struct {
	*fakeGovernor
	emitted chan struct{}
}

func (s slowGovernor) Emit(context.Context, client.DevEvent) (client.Evaluation, error) {
	time.Sleep(60 * time.Millisecond)
	close(s.emitted)
	return client.Evaluation{Verdict: client.VerdictAllow}, nil
}

// TestGate_ObserveCopySpooledWhenEscalationOutlivesItsBudget the budget-
// exceeded escalation: Escalate gives up on the transport and returns through
// its timeout branch, abandoning the goroutine that is still running.
func TestGate_ObserveCopySpooledWhenEscalationOutlivesItsBudget(t *testing.T) {
	isolateConfig(t)
	isolateMarkers(t)
	t.Setenv(devconfig.EnvTier2, "1")
	t.Setenv(devconfig.EnvApprovalHold, "50")
	t.Setenv(devconfig.EnvEnforcementFile, t.TempDir()+"/enforcements.jsonl")
	defer devconfig.Pin()()

	gov := slowGovernor{fakeGovernor: &fakeGovernor{}, emitted: make(chan struct{})}
	spooled := false
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 20 * time.Millisecond,
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record:       func(decision.Decision, ApplyResult) {},
		SpoolObserve: func() { spooled = true },
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})

	<-gov.emitted

	if !spooled {
		t.Error("the escalation was abandoned at its budget, so delivery is unknown; " +
			"dropping the observe copy risks losing the event entirely")
	}
}

// TestGate_ObserveCopySpooledOnStaleGateEarlyReturn is deleted with the stale
// gate : there is no local bundle to be stale, so no early return before the
// evaluation.

// TestGate_NilSpoolObserveIsInert a nil SpoolObserve is the non-gated caller's
// shape (it spooled its own copy). The gate must not panic on it.
func TestGate_NilSpoolObserveIsInert(t *testing.T) {
	isolateConfig(t)
	isolateMarkers(t)
	t.Setenv(devconfig.EnvTier2, "0")
	t.Setenv(devconfig.EnvEnforcementFile, t.TempDir()+"/enforcements.jsonl")
	defer devconfig.Pin()()

	var out bytes.Buffer
	gate := EnforceGate{
		Contract:  testContract{approval: "ask"},
		Evaluator: Evaluator{Ceiling: provider.HookCeiling{Gating: 30 * time.Second}},
		Record:    func(decision.Decision, ApplyResult) {},
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})
}

// TestEngine_RecordDeferredThreadsDurationBeforeSpooling suppressing the
// redundant spool copy must not take the duration stash with it. Without this,
// the fix would silently blank duration_ms for exactly the escalated calls.
func TestEngine_RecordDeferredThreadsDurationBeforeSpooling(t *testing.T) {
	dir := t.TempDir()
	e := NewEngine(dir)

	started := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       "evt-start",
		EventType:     client.EventToolCall,
		SessionID:     "sess-dur",
		DeveloperDID:  "did:aip:dev",
		Timestamp:     "2026-08-01T12:00:00Z",
		StartedAt:     "2026-08-01T12:00:00Z",
		Tool:          client.Tool{Name: "Bash", Kind: client.ToolShell},
	}

	appendObserve := e.RecordDeferred(started)

	if got := e.Durations.TakeStart(started.SessionID, ToolCallStartKey(started)); got != started.StartedAt {
		t.Errorf("duration stash = %q, want %q — suppressing the spool copy must not "+
			"cost the call its duration_ms", got, started.StartedAt)
	}
	if n := spooledLines(t, e, started.SessionID); n != 0 {
		t.Errorf("spool holds %d events before the deferred append ran, want 0", n)
	}

	if err := appendObserve(); err != nil {
		t.Fatalf("deferred append: %v", err)
	}
	if n := spooledLines(t, e, started.SessionID); n != 1 {
		t.Errorf("spool holds %d events after the deferred append, want 1", n)
	}
}

func spooledLines(t *testing.T, e *Engine, sessionID string) int {
	t.Helper()
	data, err := os.ReadFile(e.Spool.SessionPath(sessionID))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	return len(NonEmptyLines(data))
}
