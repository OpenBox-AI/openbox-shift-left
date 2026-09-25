package hookflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
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
		t.Error("the gate never polled; the request was filed but nobody waited for it")
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
			t.Errorf("no marker after an exhausted hold: %v; the late decision would land unannounced", err)
		}
	})

	t.Run("answered hold takes it away", func(t *testing.T) {
		g := &fakeGovernor{replies: []func() (client.ApprovalStatus, error){decided(client.VerdictAllow, expiry)}}
		runGate(t, g, "5000")
		if _, err := os.Stat(PendingApprovalPath(key)); err == nil {
			t.Error("marker survived a hold that answered; the watcher would repeat the outcome")
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
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
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
		t.Errorf("gate asked for a verdict %d times, want exactly 1; a retry inside the "+
			"gate amplifies a core outage across every tool call of every session", n)
	}
}

// ── Gate drain (queue set) ──────────────────────────────────────────────

// orderedGovernor records each Emit call's own event id, in call order, and
// answers per event id: an explicit error (errFor), a verdict (verdictFor,
// default ALLOW), or blocks until its own ctx is done (sleepUntilDone) to
// stand in for an attempt whose answer never arrives within its own window.
// delay, when set, holds every answer by that long first -- standing in for
// a real per-event network cost so a many-event drain pass takes real wall
// time (a many-event backlog test's own point).
type orderedGovernor struct {
	*fakeGovernor
	mu             sync.Mutex
	order          []string
	errFor         map[string]error
	verdictFor     map[string]client.Evaluation
	sleepUntilDone map[string]bool
	delay          time.Duration
}

func (g *orderedGovernor) Emit(ctx context.Context, ev client.DevEvent) (client.Evaluation, error) {
	g.mu.Lock()
	g.order = append(g.order, ev.EventID)
	g.mu.Unlock()
	if g.sleepUntilDone[ev.EventID] {
		<-ctx.Done()
		return client.Evaluation{}, ctx.Err()
	}
	if g.delay > 0 {
		select {
		case <-time.After(g.delay):
		case <-ctx.Done():
			return client.Evaluation{}, ctx.Err()
		}
	}
	if err, ok := g.errFor[ev.EventID]; ok {
		return client.Evaluation{}, err
	}
	if v, ok := g.verdictFor[ev.EventID]; ok {
		return v, nil
	}
	return client.Evaluation{Verdict: client.VerdictAllow}, nil
}

func (g *orderedGovernor) Order() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.order...)
}

func seedQueued(t *testing.T, e *Engine, sessionID, eventID string, eventType client.EventType) {
	t.Helper()
	if err := e.Spool.Append(client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       eventID,
		EventType:     eventType,
		SessionID:     sessionID,
		DeveloperDID:  "did:aip:dev",
		Timestamp:     "2026-08-01T12:00:00Z",
	}); err != nil {
		t.Fatalf("seed %s: %v", eventID, err)
	}
}

func gateDrainEnv(t *testing.T) {
	t.Helper()
	isolateConfig(t)
	isolateMarkers(t)
	t.Setenv(devconfig.EnvEnforcementFile, t.TempDir()+"/enforcements.jsonl")
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
}

// (a) a queued backlog is drained, in order, strictly before the gate's own
// escalation POST.
func TestGate_QueuedBacklogPrecedesTheGatedEscalation(t *testing.T) {
	gateDrainEnv(t)
	defer devconfig.Pin()()

	engine := NewEngine(t.TempDir())
	seedQueued(t, engine, "sess-1", "seed-started", client.EventSessionStarted)
	seedQueued(t, engine, "sess-1", "seed-1", client.EventToolCall)
	seedQueued(t, engine, "sess-1", "seed-2", client.EventToolCall)

	gov := &orderedGovernor{fakeGovernor: &fakeGovernor{}}
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 4 * time.Second,
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record: func(decision.Decision, ApplyResult) {},
		Queue:  engine,
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})

	want := []string{"seed-started", "seed-1", "seed-2", "evt-1"}
	if got := gov.Order(); !reflect.DeepEqual(got, want) {
		t.Errorf("emit order = %v, want %v (the queued backlog, in order, before this call's own escalation)", got, want)
	}
}

// (b) the escalation's own budget is unaffected by a drain that found
// nothing to do (an empty queue): a gate with Queue set must spend no more
// real time getting to its own escalation than one with no queue at all.
func TestGate_EscalationBudgetUnchangedByAnEmptyDrain(t *testing.T) {
	gateDrainEnv(t)
	defer devconfig.Pin()()

	for _, withQueue := range []bool{false, true} {
		t.Run(fmt.Sprintf("Queue set=%v", withQueue), func(t *testing.T) {
			gov := slowGovernor{fakeGovernor: &fakeGovernor{}, emitted: make(chan struct{})}
			var out bytes.Buffer
			gate := EnforceGate{
				Contract: testContract{approval: "ask"},
				Evaluator: Evaluator{
					Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
					MaxTimeout: 30 * time.Millisecond,
					NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
				},
				Record: func(decision.Decision, ApplyResult) {},
			}
			if withQueue {
				gate.Queue = NewEngine(t.TempDir())
			}
			start := time.Now()
			gate.Run(context.Background(), discard(), &out, shellTarget{})
			if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
				t.Errorf("gate.Run took %v to give up on its own escalation; Queue being set must not shrink its budget", elapsed)
			}
			<-gov.emitted
		})
	}
}

// (c) a delivery failure this call's own drain step finds denies the
// draining call itself, through OnHalted, with zero escalation POSTs.
func TestGate_DrainedDeliveryFailureDeniesWithoutEscalating(t *testing.T) {
	gateDrainEnv(t)
	defer devconfig.Pin()()

	engine := NewEngine(t.TempDir())
	seedQueued(t, engine, "sess-1", "seed-started", client.EventSessionStarted)

	gov := &orderedGovernor{fakeGovernor: &fakeGovernor{}, errFor: map[string]error{"seed-started": client.ErrDelivery}}
	var onHaltedCalled bool
	var onHaltedInfo SessionHaltInfo
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 4 * time.Second,
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record: func(decision.Decision, ApplyResult) {},
		Queue:  engine,
		OnHalted: func(info SessionHaltInfo) ApplyResult {
			onHaltedCalled = true
			onHaltedInfo = info
			return ApplyResult{Decision: DecisionDeny, Emitted: true}
		},
	}
	res := gate.Run(context.Background(), discard(), &out, shellTarget{})

	if !onHaltedCalled {
		t.Fatal("a delivery failure found while draining must render through OnHalted")
	}
	if res.Decision != DecisionDeny {
		t.Errorf("Run's own return = %+v, want the OnHalted render", res)
	}
	if got := gov.Order(); !reflect.DeepEqual(got, []string{"seed-started"}) {
		t.Errorf("emit order = %v, want exactly the drained backlog event; the gate's own escalation must never run", got)
	}
	if onHaltedInfo.EventType != string(client.EventSessionStarted) {
		t.Errorf("latch EventType = %q, want %q", onHaltedInfo.EventType, client.EventSessionStarted)
	}
}

// (d) a HALT verdict this call's own drain step finds denies the draining
// call the same way, through the same latch and the same OnHalted render.
func TestGate_DrainedHaltVerdictDeniesWithoutEscalating(t *testing.T) {
	gateDrainEnv(t)
	defer devconfig.Pin()()

	engine := NewEngine(t.TempDir())
	seedQueued(t, engine, "sess-1", "seed-started", client.EventSessionStarted)

	gov := &orderedGovernor{fakeGovernor: &fakeGovernor{}, verdictFor: map[string]client.Evaluation{
		"seed-started": {Verdict: client.VerdictHalt, Reason: "org kill switch", PolicyID: "p-1"},
	}}
	var onHaltedCalled bool
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 4 * time.Second,
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record: func(decision.Decision, ApplyResult) {},
		Queue:  engine,
		OnHalted: func(info SessionHaltInfo) ApplyResult {
			onHaltedCalled = true
			return ApplyResult{Decision: DecisionDeny, Emitted: true}
		},
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})

	if !onHaltedCalled {
		t.Fatal("a HALT verdict found while draining must render through OnHalted")
	}
	if got := gov.Order(); !reflect.DeepEqual(got, []string{"seed-started"}) {
		t.Errorf("emit order = %v, want exactly the drained backlog event; the gate's own escalation must never run", got)
	}
	if _, halted := SessionHalted("sess-1"); !halted {
		t.Error("a HALT verdict found while draining must latch the run")
	}
}

// (e) an exhausted slack skips the drain entirely: the backlog stays queued,
// untouched, and the gate's own escalation still runs, un-latched.
func TestGate_ExhaustedSlackSkipsTheDrain(t *testing.T) {
	gateDrainEnv(t)
	defer devconfig.Pin()()

	engine := NewEngine(t.TempDir())
	seedQueued(t, engine, "sess-1", "seed-1", client.EventToolCall)

	gov := &orderedGovernor{fakeGovernor: &fakeGovernor{}}
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			// Gating - HookBudgetMargin(1s) == DefaultEvaluationTimeout
			// (10s): the escalation's own budget consumes essentially all
			// of it, leaving ~0 slack for the drain -- well under
			// GateDrainAttemptTimeout (3.5s), so the drain is skipped.
			Ceiling:    provider.HookCeiling{Gating: DefaultEvaluationTimeout + HookBudgetMargin},
			MaxTimeout: 10 * time.Second,
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record: func(decision.Decision, ApplyResult) {},
		Queue:  engine,
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})

	if got := gov.Order(); !reflect.DeepEqual(got, []string{"evt-1"}) {
		t.Errorf("emit order = %v, want only the gate's own escalation; an exhausted slack must not attempt the backlog", got)
	}
	if n := engine.Spool.PendingCount("sess-1"); n != 1 {
		t.Errorf("pending count = %d, want 1; the untouched backlog must stay queued", n)
	}
	if _, halted := SessionHalted("sess-1"); halted {
		t.Error("skipping the drain must never latch the run")
	}
}

// (f) a stripe already held by another drainer past this call's own slack
// behaves exactly like (e): the backlog stays queued, and the gate's own
// escalation still runs.
func TestGate_BusyStripePastSlackSkipsTheDrain(t *testing.T) {
	gateDrainEnv(t)
	defer devconfig.Pin()()

	engine := NewEngine(t.TempDir())
	seedQueued(t, engine, "sess-1", "seed-1", client.EventToolCall)

	fl := flock.New(engine.Spool.sessionStripePath("sess-1"))
	if ok, err := fl.TryLock(); err != nil || !ok {
		t.Fatalf("could not take the stripe lock ahead of the gate: ok=%v err=%v", ok, err)
	}
	defer fl.Unlock()

	gov := &orderedGovernor{fakeGovernor: &fakeGovernor{}}
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			// Gating - HookBudgetMargin - the escalation's own budget still
			// leaves a slack (~4.5s) comfortably above GateDrainAttemptTimeout
			// (3.5s), so the drain is attempted (and blocks on the busy stripe)
			// rather than skipped outright for being too small to cover one
			// attempt.
			Ceiling:    provider.HookCeiling{Gating: 6 * time.Second},
			MaxTimeout: 500 * time.Millisecond,
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record: func(decision.Decision, ApplyResult) {},
		Queue:  engine,
	}
	start := time.Now()
	gate.Run(context.Background(), discard(), &out, shellTarget{})
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Errorf("gate.Run took %v; a busy stripe must not be waited on past this call's own slack", elapsed)
	}

	if got := gov.Order(); !reflect.DeepEqual(got, []string{"evt-1"}) {
		t.Errorf("emit order = %v, want only the gate's own escalation; a busy stripe must not attempt the backlog", got)
	}
	if n := engine.Spool.PendingCount("sess-1"); n != 1 {
		t.Errorf("pending count = %d, want 1; the untouched backlog must stay queued", n)
	}
}

// stripeHoldingEmitter's Emit holds its own stripe (DrainSession holds the lock
// across the whole call) for exactly hold, and closes started the instant it
// is actually invoked -- which only happens once it holds the lock -- so a
// caller can synchronize on "the stripe is now genuinely held" instead of a
// fixed sleep guessing at it.
type stripeHoldingEmitter struct {
	started chan struct{}
	once    sync.Once
	hold    time.Duration
}

func (e *stripeHoldingEmitter) Emit(ctx context.Context, _ client.DevEvent) (client.Evaluation, error) {
	e.once.Do(func() { close(e.started) })
	select {
	case <-time.After(e.hold):
	case <-ctx.Done():
	}
	return client.Evaluation{Verdict: client.VerdictAllow}, nil
}

// TestGate_StripeHeldPastMaxStripeWaitSkipsTheDrainAndEscalatesPromptly pins
// MaxStripeWait's own bound directly: a background drainer holds this
// session's stripe for 8s (a live detached flusher or lane daemon mid-
// delivery on one slow event), well past MaxStripeWait (5s). The gate's own
// drain step must give up on the LOCK within ~MaxStripeWait and proceed to
// its own escalation, never waited on for anywhere near the full 8s hold.
func TestGate_StripeHeldPastMaxStripeWaitSkipsTheDrainAndEscalatesPromptly(t *testing.T) {
	gateDrainEnv(t)
	defer devconfig.Pin()()

	engine := NewEngine(t.TempDir())
	seedQueued(t, engine, "sess-1", "seed-1", client.EventToolCall)

	be := &stripeHoldingEmitter{started: make(chan struct{}), hold: 8 * time.Second}
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		_, _ = engine.DrainSession(context.Background(), "sess-1", be, DrainOptions{
			Mode: Block, AttemptTimeout: 10 * time.Second,
		})
	}()
	<-be.started // the background drainer now genuinely holds the stripe
	defer func() { <-drainDone }()

	gov := &orderedGovernor{fakeGovernor: &fakeGovernor{}}
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 4 * time.Second,
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record: func(decision.Decision, ApplyResult) {},
		Queue:  engine,
	}
	start := time.Now()
	gate.Run(context.Background(), discard(), &out, shellTarget{})
	if elapsed := time.Since(start); elapsed >= 5500*time.Millisecond {
		t.Errorf("gate.Run took %v, want < 5.5s: a stripe held past MaxStripeWait must never be waited on that long", elapsed)
	}

	if got := gov.Order(); !reflect.DeepEqual(got, []string{"evt-1"}) {
		t.Errorf("emit order = %v, want only the gate's own escalation; a stripe held past MaxStripeWait must not attempt the backlog", got)
	}
	// PendingCount is deliberately NOT asserted here: the background drainer
	// already rotated "seed-1" out of the visible tail into its own
	// in-flight attempt the instant it took the stripe (collectSession, held
	// only in memory until its own Emit returns), so the queue reads as
	// EMPTY during this exact window even though nothing has been
	// delivered yet -- gov.Order() above is what actually proves the gate's
	// own client never touched it.
}

// TestGate_UncontendedBacklogStillDrainsPastMaxStripeWaitWhenSlackAllows is
// MaxStripeWait's own negative space: the cap bounds ONLY the wait to
// ACQUIRE an uncontended stripe (acquired at once here, nothing to wait
// for), never the drain PASS itself once held. 60 events at a real 100ms
// each is 6s of genuine delivery time -- past MaxStripeWait -- and every one
// of them must still be delivered, in order, before the gate's own
// escalation: a large uncontended backlog must never be cut short at 5s
// merely because MaxStripeWait happens to be 5s too.
func TestGate_UncontendedBacklogStillDrainsPastMaxStripeWaitWhenSlackAllows(t *testing.T) {
	gateDrainEnv(t)
	defer devconfig.Pin()()

	engine := NewEngine(t.TempDir())
	const n = 60
	for i := 0; i < n; i++ {
		seedQueued(t, engine, "sess-1", fmt.Sprintf("seed-%02d", i), client.EventToolCall)
	}

	gov := &orderedGovernor{fakeGovernor: &fakeGovernor{}, delay: 100 * time.Millisecond}
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 4 * time.Second,
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record: func(decision.Decision, ApplyResult) {},
		Queue:  engine,
	}
	start := time.Now()
	gate.Run(context.Background(), discard(), &out, shellTarget{})
	if elapsed := time.Since(start); elapsed < 6*time.Second {
		t.Errorf("gate.Run took only %v, want >= 6s (60 events x 100ms of real, uncontended delivery time): "+
			"the pass was cut short, as if MaxStripeWait capped the drain itself rather than only the lock wait", elapsed)
	}

	got := gov.Order()
	if len(got) != n+1 {
		t.Fatalf("emit count = %d, want %d (the whole backlog, then the gate's own escalation)", len(got), n+1)
	}
	for i := 0; i < n; i++ {
		if want := fmt.Sprintf("seed-%02d", i); got[i] != want {
			t.Errorf("emit[%d] = %q, want %q; the backlog must drain in order", i, got[i], want)
		}
	}
	if got[n] != "evt-1" {
		t.Errorf("last emit = %q, want the gate's own escalation event", got[n])
	}
}

// (g) NewClient is built at most once per Run, whether or not the drain,
// the escalation and the approval hold all end up running.
func TestGate_NewClientBuiltOncePerRun(t *testing.T) {
	gateDrainEnv(t)
	t.Setenv(devconfig.EnvTier2, "1")
	t.Setenv(devconfig.EnvApprovalHold, "50")
	defer devconfig.Pin()()

	engine := NewEngine(t.TempDir())
	seedQueued(t, engine, "sess-1", "seed-1", client.EventToolCall)

	expiry := time.Now().Add(time.Hour)
	inner := &fakeGovernor{replies: []func() (client.ApprovalStatus, error){
		pending(expiry), decided(client.VerdictAllow, expiry),
	}}
	gov := approvalGovernor{fakeGovernor: inner}

	var builds int32
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 4 * time.Second,
			NewClient: func(*log.Logger) (Governor, error) {
				atomic.AddInt32(&builds, 1)
				return gov, nil
			},
		},
		Record: func(decision.Decision, ApplyResult) {},
		Queue:  engine,
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})

	if n := atomic.LoadInt32(&builds); n != 1 {
		t.Errorf("NewClient built %d times, want exactly 1 for drain + escalation + approval hold together", n)
	}
	if inner.polls == 0 {
		t.Error("the approval hold never ran; this test would pass vacuously without it")
	}
}

// (h) Queue == nil must skip the drain and the post-drain latch re-read
// entirely, even when OnHalted/ForceFlush are (mistakenly) wired and a
// latch already exists for this run: behavior byte-identical to a gate that
// has never heard of a queue.
func TestGate_QueueNilNeverConsultsHaltOrForcesFlush(t *testing.T) {
	gateDrainEnv(t)
	defer devconfig.Pin()()

	WriteSessionHalt(discard(), "sess-1", client.Evaluation{Reason: "unrelated"})

	var onHaltedCalled, forceFlushCalled bool
	gov := &fakeGovernor{}
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 4 * time.Second,
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record:     func(decision.Decision, ApplyResult) {},
		OnHalted:   func(SessionHaltInfo) ApplyResult { onHaltedCalled = true; return ApplyResult{} },
		ForceFlush: func() { forceFlushCalled = true },
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})

	if onHaltedCalled {
		t.Error("OnHalted was called with Queue == nil; an existing latch must be irrelevant to a gate with no queue wired")
	}
	if forceFlushCalled {
		t.Error("ForceFlush was called with Queue == nil")
	}
}

// TestGate_SmallSlackNeverTakesTheStripeOrAttempts a slack too small to
// cover even one full evaluation must never take the session's own stripe
// lock or fire a doomed-to-time-out attempt: skipped entirely, exactly like
// an exhausted (<=0) slack.
func TestGate_SmallSlackNeverTakesTheStripeOrAttempts(t *testing.T) {
	gateDrainEnv(t)
	defer devconfig.Pin()()

	engine := NewEngine(t.TempDir())
	seedQueued(t, engine, "sess-1", "seed-1", client.EventToolCall)

	gov := &orderedGovernor{fakeGovernor: &fakeGovernor{}}
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			// Gating - HookBudgetMargin - the escalation's own budget leaves
			// a slack of only ~20ms: far short of one full evaluation.
			Ceiling:    provider.HookCeiling{Gating: DefaultEvaluationTimeout + HookBudgetMargin + 20*time.Millisecond},
			MaxTimeout: 10 * time.Second,
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record: func(decision.Decision, ApplyResult) {},
		Queue:  engine,
	}
	gate.Run(context.Background(), discard(), &out, shellTarget{})

	if got := gov.Order(); !reflect.DeepEqual(got, []string{"evt-1"}) {
		t.Errorf("emit order = %v, want only the gate's own escalation; a slack too small for one attempt must never fire one", got)
	}
	if n := engine.Spool.PendingCount("sess-1"); n != 1 {
		t.Errorf("pending count = %d, want 1; the untouched backlog must stay queued", n)
	}
	if _, err := os.Stat(engine.Spool.sessionStripePath("sess-1")); !os.IsNotExist(err) {
		t.Errorf("the session's stripe lock file exists (stat err=%v); a slack too small for one attempt must never even take the lock", err)
	}
}

// TestGate_LargeSlackStillDrainsABacklog is the happy-path counterpart to
// the small-slack test above: a generous (30s-ceiling) slack still drains a
// real backlog, N>0, exactly as before the fix.
func TestGate_LargeSlackStillDrainsABacklog(t *testing.T) {
	gateDrainEnv(t)
	defer devconfig.Pin()()

	engine := NewEngine(t.TempDir())
	const n = 50
	for i := 0; i < n; i++ {
		seedQueued(t, engine, "sess-1", fmt.Sprintf("seed-%02d", i), client.EventToolCall)
	}

	gov := &orderedGovernor{fakeGovernor: &fakeGovernor{}}
	var logBuf bytes.Buffer
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 4 * time.Second,
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record: func(decision.Decision, ApplyResult) {},
		Queue:  engine,
	}
	gate.Run(context.Background(), log.New(&logBuf, "", 0), &out, shellTarget{})

	got := gov.Order()
	if len(got) != n+1 {
		t.Fatalf("emit count = %d, want %d (the whole backlog, then the gate's own escalation)", len(got), n+1)
	}
	for i := 0; i < n; i++ {
		if want := fmt.Sprintf("seed-%02d", i); got[i] != want {
			t.Errorf("emit[%d] = %q, want %q; the backlog must drain in order", i, got[i], want)
		}
	}
	if got[n] != "evt-1" {
		t.Errorf("last emit = %q, want the gate's own escalation event", got[n])
	}
	if !strings.Contains(logBuf.String(), "drained 50") {
		t.Errorf("log = %q, want a \"drained 50\" line", logBuf.String())
	}
}

// TestGate_HaltedWithNoOnHaltedStillDeniesWithoutEscalating a latched run
// must never be re-asked, even when the caller wired Queue but no OnHalted
// closure: the gate itself renders a generic fail-closed deny carrying the
// latch's own reason, rather than silently falling through to escalate.
func TestGate_HaltedWithNoOnHaltedStillDeniesWithoutEscalating(t *testing.T) {
	gateDrainEnv(t)
	defer devconfig.Pin()()

	WriteSessionHalt(discard(), "sess-1", client.Evaluation{Reason: "org kill switch", PolicyID: "p-1"})

	engine := NewEngine(t.TempDir())
	gov := &orderedGovernor{fakeGovernor: &fakeGovernor{}}
	var recorded decision.Decision
	var out bytes.Buffer
	gate := EnforceGate{
		Contract: testContract{approval: "ask"},
		Evaluator: Evaluator{
			Ceiling:    provider.HookCeiling{Gating: 30 * time.Second},
			MaxTimeout: 4 * time.Second,
			NewClient:  func(*log.Logger) (Governor, error) { return gov, nil },
		},
		Record: func(dec decision.Decision, _ ApplyResult) { recorded = dec },
		Queue:  engine,
		// OnHalted deliberately left nil.
	}
	res := gate.Run(context.Background(), discard(), &out, shellTarget{})

	if len(gov.Order()) != 0 {
		t.Errorf("emit order = %v, want none; a latched run must never be re-asked", gov.Order())
	}
	if res.Decision != DecisionHalt && res.Decision != DecisionDeny {
		t.Errorf("applied decision = %q, want a deny/halt", res.Decision)
	}
	if !strings.Contains(recorded.Evaluation.Reason, "org kill switch") {
		t.Errorf("recorded reason = %q, want the latch's own reason", recorded.Evaluation.Reason)
	}
}
