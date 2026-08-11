package hookflow

import (
	"bytes"
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// The invariant these tests pin: ONE gated tool call puts exactly ONE
// ActivityStarted on the wire.
//
// A gated PreToolUse has two ways to reach core, and they carry the SAME event
// (one event_id, because the mapper clock is pinned for the hook invocation):
// the Tier-2 escalation POSTs it synchronously, and the observe copy is spooled
// and flushed. Core does not dedupe developer events on their id, so whichever
// of the two runs, the other must not — otherwise one tool call becomes two
// stored rows and two Merkle leaves. Both halves used to run, so every escalated
// call was double-counted.
//
// Direction of failure matters: when in doubt, spool. A redundant spool copy is
// wrong; a missing one loses telemetry outright.

// deliveringGate builds the gate under test with a Governor whose Emit outcome
// the caller chooses, and reports whether the observe copy was spooled.
func deliveringGate(t *testing.T, gov Governor, tier2Enabled string) (spooled bool) {
	t.Helper()
	isolateConfig(t)
	t.Setenv(devconfig.EnvTier2, tier2Enabled)
	// A short hold: these tests assert what gets spooled, not how long the gate
	// is willing to wait for an approver.
	t.Setenv(devconfig.EnvApprovalHold, "50")
	t.Setenv(devconfig.EnvEnforcementFile, t.TempDir()+"/enforcements.jsonl")
	isolateMarkers(t)
	defer devconfig.Pin()()

	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Tier2: Tier2{
			HookBudget: 29 * time.Second,
			MaxTimeout: 4 * time.Second,
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record:       func(decision.Decision, ApplyResult) {},
		SpoolObserve: func() { spooled = true },
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})
	return spooled
}

// The bug, in one assertion. The escalation delivered the event, so spooling a
// second copy of it is what produced the duplicate ActivityStarted.
//
// fakeGovernor's Emit succeeds but returns an EMPTY evaluation — delivered, yet
// no usable verdict, so the escalation still degrades to fail-open. That is the
// case a check on the resulting decision would get wrong: the decision reads
// "tier-2 returned no verdict" while the event is already stored. Delivery is a
// property of the transport, not of the answer.
func TestGate_ObserveCopySkippedWhenEscalationDelivered(t *testing.T) {
	gov := &fakeGovernor{}
	if deliveringGate(t, gov, "1") {
		t.Error("escalation delivered this event; spooling it again stores a second " +
			"ActivityStarted for one tool call (core does not dedupe)")
	}
}

// The fail-open safety net, and the reason suppression must key on the transport
// rather than on "did we try": nothing reached core, so the spool copy is the
// only remaining path for this telemetry and must survive.
func TestGate_ObserveCopySpooledWhenEscalationUndelivered(t *testing.T) {
	gov := degradedGovernor{fakeGovernor: &fakeGovernor{}}
	if !deliveringGate(t, gov, "1") {
		t.Error("escalation never delivered; dropping the observe copy loses the event entirely")
	}
}

// A REQUIRE_APPROVAL verdict is delivered like any other — it is a filed record,
// which is precisely a stored row. The hold that follows must not un-suppress
// the observe copy.
func TestGate_ObserveCopySkippedWhenApprovalWasFiled(t *testing.T) {
	gov := approvalGovernor{fakeGovernor: &fakeGovernor{
		replies: []func() (client.ApprovalStatus, error){pending(time.Now().Add(time.Hour))},
	}}
	if deliveringGate(t, gov, "1") {
		t.Error("the filed approval IS the stored event; the observe copy duplicates it")
	}
}

// With Tier-2 off there is no second path, so the gate owes the spool a copy.
// This is the default posture, and it must behave exactly like observe-only.
func TestGate_ObserveCopySpooledWhenTier2Disabled(t *testing.T) {
	gov := &fakeGovernor{}
	if !deliveringGate(t, gov, "0") {
		t.Error("no escalation ran; the observe copy is the only copy and must be spooled")
	}
}

// The stale gate returns before the escalation is even reached. Deferring the
// spool write must not let an early return swallow it — every exit path owes the
// spool a copy unless delivery actually happened.
func TestGate_ObserveCopySpooledOnStaleGateEarlyReturn(t *testing.T) {
	isolateConfig(t)
	isolateMarkers(t)
	t.Setenv(devconfig.EnvTier2, "1")
	t.Setenv(devconfig.EnvFailClosed, "1")
	t.Setenv(devconfig.EnvEnforcementFile, t.TempDir()+"/enforcements.jsonl")
	t.Setenv(EnvStaleDir, t.TempDir())
	if err := WriteStaleMarker(shellTarget{}.SessionID()); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}
	defer devconfig.Pin()()

	spooled := false
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Tier2: Tier2{
			HookBudget: 29 * time.Second,
			MaxTimeout: 4 * time.Second,
			NewClient: func(*log.Logger) (Governor, error) {
				t.Error("stale gate must deny before any escalation")
				return &fakeGovernor{}, nil
			},
		},
		Record:       func(decision.Decision, ApplyResult) {},
		SpoolObserve: func() { spooled = true },
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})

	if !spooled {
		t.Error("the stale-gate early return skipped the observe copy; a denied call is " +
			"still a call that happened and still owes telemetry")
	}
}

// A nil SpoolObserve is the non-gated caller's shape (it spooled its own copy).
// The gate must not panic on it.
func TestGate_NilSpoolObserveIsInert(t *testing.T) {
	isolateConfig(t)
	isolateMarkers(t)
	t.Setenv(devconfig.EnvTier2, "0")
	t.Setenv(devconfig.EnvEnforcementFile, t.TempDir()+"/enforcements.jsonl")
	defer devconfig.Pin()()

	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Tier2:    Tier2{HookBudget: 29 * time.Second},
		Record:   func(decision.Decision, ApplyResult) {},
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})
}

// Suppressing the redundant spool copy must not take the duration stash with it.
// The stash is what lets the PostToolUse half recover a start time and report a
// real duration_ms; it is written on the same call that used to do the spooling,
// so the split has to keep the stash write unconditional. Without this, the fix
// would silently blank duration_ms for exactly the escalated calls.
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

	// The stash is written immediately, even though nothing has been spooled.
	if got := e.Durations.TakeStart(started.SessionID, ToolCallStartKey(started)); got != started.StartedAt {
		t.Errorf("duration stash = %q, want %q — suppressing the spool copy must not "+
			"cost the call its duration_ms", got, started.StartedAt)
	}
	if n := spooledLines(t, e, started.SessionID); n != 0 {
		t.Errorf("spool holds %d events before the deferred append ran, want 0", n)
	}

	// And the returned closure is what actually spools, when the caller decides to.
	if err := appendObserve(); err != nil {
		t.Fatalf("deferred append: %v", err)
	}
	if n := spooledLines(t, e, started.SessionID); n != 1 {
		t.Errorf("spool holds %d events after the deferred append, want 1", n)
	}
}

// spooledLines counts events sitting in the session's live spool file. Distinct
// from Spool.UndeliveredCount, which reports only carry-over from a failed
// flush.
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
