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
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
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

// TestGate_ExplicitDeliveryFailureLatchesAndSkipsTheObserveCopy a
// non-transient failure of the escalation (401, 429, other 4xx, or an event
// that could not be built) earns no retry: it halts the run
// (HaltOnDeliveryFailure) rather than re-spooling a local copy. A transient
// failure instead requeues the copy for the drainers
// (TestGate_TransientEscalationFailureRequeuesTheObserveCopy).
func TestGate_ExplicitDeliveryFailureLatchesAndSkipsTheObserveCopy(t *testing.T) {
	gov := degradedGovernor{fakeGovernor: &fakeGovernor{}}
	if deliveringGate(t, gov, "1") {
		t.Error("an explicit delivery failure must not re-spool the observe copy; it is already latched and the event already had its one attempt")
	}
	if _, halted := SessionHalted("sess-1"); !halted {
		t.Error("an explicit, proven non-acceptance must halt the run")
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

// TestGate_ObserveCopyQueuedToHeadWhenEscalationOutlivesItsBudget the budget-
// exceeded escalation: Escalate gives up on the transport and returns through
// its timeout branch, abandoning the goroutine that is still running. Its
// outcome is unknown (not a proven failure), so it must be requeued ahead of
// the tail for a drainer with a real attempt (SpoolObserveHead) rather than
// spooled as an ordinary tail append (which the plain SpoolObserve callback
// must NOT receive here) or dropped.
func TestGate_ObserveCopyQueuedToHeadWhenEscalationOutlivesItsBudget(t *testing.T) {
	isolateConfig(t)
	isolateMarkers(t)
	t.Setenv(devconfig.EnvTier2, "1")
	t.Setenv(devconfig.EnvApprovalHold, "50")
	t.Setenv(devconfig.EnvEnforcementFile, t.TempDir()+"/enforcements.jsonl")
	defer devconfig.Pin()()

	gov := slowGovernor{fakeGovernor: &fakeGovernor{}, emitted: make(chan struct{})}
	var spooledTail, spooledHead bool
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 20 * time.Millisecond,
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record:           func(decision.Decision, ApplyResult) {},
		SpoolObserve:     func() { spooledTail = true },
		SpoolObserveHead: func() { spooledHead = true },
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})

	<-gov.emitted

	if !spooledHead {
		t.Error("the escalation was abandoned at its budget, so delivery is unknown; " +
			"dropping the observe copy risks losing the event entirely")
	}
	if spooledTail {
		t.Error("an unanswered escalation must be queued to the head, not appended to the ordinary tail")
	}
}

// TestGate_UnansweredEscalationGoesToHeadWhenTheDrainLeftNothingBehind is the
// same budget-exceeded escalation as above, but with a queue wired and
// nothing in it: the drain (or the lack of anything to drain) already
// leaves nothing behind, so the head is exactly where the tail is, and the
// unanswered outcome still goes there.
func TestGate_UnansweredEscalationGoesToHeadWhenTheDrainLeftNothingBehind(t *testing.T) {
	isolateConfig(t)
	isolateMarkers(t)
	t.Setenv(devconfig.EnvTier2, "1")
	t.Setenv(devconfig.EnvApprovalHold, "50")
	t.Setenv(devconfig.EnvEnforcementFile, t.TempDir()+"/enforcements.jsonl")
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	defer devconfig.Pin()()

	engine := NewEngine(t.TempDir())

	gov := slowGovernor{fakeGovernor: &fakeGovernor{}, emitted: make(chan struct{})}
	var spooledTail, spooledHead bool
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 20 * time.Millisecond,
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record:           func(decision.Decision, ApplyResult) {},
		SpoolObserve:     func() { spooledTail = true },
		SpoolObserveHead: func() { spooledHead = true },
		Queue:            engine,
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})

	<-gov.emitted

	if !spooledHead {
		t.Error("an empty queue leaves nothing behind; the unanswered outcome must still go to the head")
	}
	if spooledTail {
		t.Error("an empty queue must not route an unanswered outcome to the tail")
	}
}

// TestGate_UnansweredEscalationGoesToTailWhenTheDrainLeftSomethingBehind an
// undrained predecessor of the SAME session is still queued behind this
// call's own drain step (its own attempt never got an answer either, so it
// was correctly requeued rather than scored a failure): this call's own
// unanswered escalation must NOT be queued to the head, which would let it
// overtake that predecessor. It goes to the tail instead, same as an
// ordinary first attempt.
func TestGate_UnansweredEscalationGoesToTailWhenTheDrainLeftSomethingBehind(t *testing.T) {
	isolateConfig(t)
	isolateMarkers(t)
	t.Setenv(devconfig.EnvTier2, "1")
	t.Setenv(devconfig.EnvEnforcementFile, t.TempDir()+"/enforcements.jsonl")
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	defer devconfig.Pin()()

	engine := NewEngine(t.TempDir())
	seedQueued(t, engine, "sess-1", "seed-1", client.EventToolCall)

	// Both the queued predecessor and this call's own escalation event never
	// hear back within their own attempt window.
	gov := &orderedGovernor{fakeGovernor: &fakeGovernor{}, sleepUntilDone: map[string]bool{
		"seed-1": true, "evt-1": true,
	}}
	var spooledTail, spooledHead bool
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 20 * time.Millisecond,
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record:           func(decision.Decision, ApplyResult) {},
		SpoolObserve:     func() { spooledTail = true },
		SpoolObserveHead: func() { spooledHead = true },
		Queue:            engine,
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})

	if !spooledTail {
		t.Error("an undrained predecessor must route this call's own unanswered event to the tail, not the head")
	}
	if spooledHead {
		t.Error("routing to the head here would let this call's own event overtake its undrained predecessor")
	}
	if n := engine.Spool.PendingCount("sess-1"); n == 0 {
		t.Error("the predecessor must still be queued (requeued, not delivered) for this to be a meaningful test")
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

	if rec := e.Durations.takePair(started.SessionID, pairKey(started)); rec.StartedAt != started.StartedAt {
		t.Errorf("duration stash = %q, want %q; suppressing the spool copy must not "+
			"cost the call its duration_ms", rec.StartedAt, started.StartedAt)
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

// TestGate_UnansweredEscalationGoesToTailWhenASameSessionAppendLandsDuringEscalation
// a lane daemon shares this session's own spool and can append a new event
// for it WHILE this call's own escalation is still in flight: the routing
// decision (tail vs. head) must be re-read at the time the outcome is
// actually dispatched, not decided from a snapshot taken before the
// escalation ran, or a same-session append landing mid-escalation would be
// silently overtaken by this call's own event going to the head.
func TestGate_UnansweredEscalationGoesToTailWhenASameSessionAppendLandsDuringEscalation(t *testing.T) {
	isolateConfig(t)
	isolateMarkers(t)
	t.Setenv(devconfig.EnvTier2, "1")
	t.Setenv(devconfig.EnvEnforcementFile, t.TempDir()+"/enforcements.jsonl")
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	defer devconfig.Pin()()

	engine := NewEngine(t.TempDir())
	// The queue starts EMPTY: a snapshot taken before the escalation runs
	// would read "nothing to overtake" and stay that way for the rest of
	// the call.

	gov := &appendingGovernor{fakeGovernor: &fakeGovernor{}, engine: engine, sessionID: "sess-1", eventID: "landed-mid-escalation"}
	var spooledTail, spooledHead bool
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 20 * time.Millisecond,
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record:           func(decision.Decision, ApplyResult) {},
		SpoolObserve:     func() { spooledTail = true },
		SpoolObserveHead: func() { spooledHead = true },
		Queue:            engine,
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})

	if !spooledTail {
		t.Error("a same-session append that landed during the escalation must route this call's own unanswered event to the tail")
	}
	if spooledHead {
		t.Error("routing to the head here would let this call's own event overtake the append that landed during the escalation")
	}
}

// appendingGovernor's Emit appends a fresh event to the given session's
// spool (simulating a lane daemon sharing it) and then blocks until its own
// ctx is done, so the caller's escalation always ends up EscalationUnanswered.
type appendingGovernor struct {
	*fakeGovernor
	engine    *Engine
	sessionID string
	eventID   string
}

func (g *appendingGovernor) Emit(ctx context.Context, ev client.DevEvent) (client.Evaluation, error) {
	_ = g.engine.Spool.Append(client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       g.eventID,
		EventType:     client.EventToolCall,
		SessionID:     g.sessionID,
		DeveloperDID:  "did:aip:dev",
		Timestamp:     "2026-08-01T12:00:00Z",
	})
	<-ctx.Done()
	return client.Evaluation{}, ctx.Err()
}
