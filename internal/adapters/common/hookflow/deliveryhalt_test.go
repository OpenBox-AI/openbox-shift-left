package hookflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

func devEventFor(sessionID string, eventType client.EventType) client.DevEvent {
	return client.DevEvent{EventID: "e-" + sessionID, EventType: eventType, SessionID: sessionID}
}

// TestHaltOnDeliveryFailure_LatchesAndNamesTheClass is R1/R2: a single
// explicit failure both latches the run (write-if-absent) and names the
// event and the failure class in the preserved reason, content-free.
func TestHaltOnDeliveryFailure_LatchesAndNamesTheClass(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	ev := devEventFor("run-explicit", client.EventToolCall)
	err := fmt.Errorf("%w: dial tcp: connection refused", client.ErrDelivery)

	HaltOnDeliveryFailure(nopLogger(), ev, err)

	info, halted := SessionHalted("run-explicit")
	if !halted {
		t.Fatal("an explicit delivery failure must latch the run")
	}
	if info.Cause != client.FailureClass(err) {
		t.Errorf("latch Cause = %q, want %q", info.Cause, client.FailureClass(err))
	}
	if info.EventType != string(client.EventToolCall) {
		t.Errorf("latch EventType = %q, want %q", info.EventType, client.EventToolCall)
	}
	if info.TS == "" {
		t.Error("latch carries no timestamp")
	}
	wantSubstrings := []string{"ToolCall", client.FailureClass(err), "halted", "new session"}
	for _, s := range wantSubstrings {
		if !strings.Contains(info.Reason, s) {
			t.Errorf("reason %q missing %q", info.Reason, s)
		}
	}
}

// TestHaltOnDeliveryFailure_OrphanedDrainNamedDistinctly a drainer that died
// mid-delivery is not misreported as a network fault: its own class,
// "orphaned", is what the latch preserves.
func TestHaltOnDeliveryFailure_OrphanedDrainNamedDistinctly(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	ev := devEventFor("run-orphan", client.EventSessionEnded)
	HaltOnDeliveryFailure(nopLogger(), ev, errOrphanedDrain)

	info, halted := SessionHalted("run-orphan")
	if !halted {
		t.Fatal("a reclaimed orphan must latch the run")
	}
	if info.Cause != "orphaned" {
		t.Errorf("Cause = %q, want %q (never a misleading network/timeout guess)", info.Cause, "orphaned")
	}
}

// TestHaltOnDeliveryFailure_RunIDPreferredOverSessionID mirrors Deliver's own
// selection: a continued run latches on RunID, not SessionID.
func TestHaltOnDeliveryFailure_RunIDPreferredOverSessionID(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	ev := client.DevEvent{EventID: "e1", EventType: client.EventToolCall, SessionID: "sess-continued", RunID: "run-xyz"}
	HaltOnDeliveryFailure(nopLogger(), ev, errors.New("boom"))

	if _, halted := SessionHalted("run-xyz"); !halted {
		t.Error("a record carrying RunID must latch the RUN, not the session id")
	}
	if _, halted := SessionHalted("sess-continued"); halted {
		t.Error("the bare session id must not read halted; only the run id was latched")
	}
}

// TestHaltOnDeliveryFailure_NeverOverwritesAnExistingLatch is R1's write-if-
// absent guarantee from the OTHER direction: a live HALT verdict (an
// entirely different origin, WriteSessionHalt) already latched this run;
// a LATER delivery failure for the same run must leave it exactly as it
// was -- the run's first cause is what every later call sees.
func TestHaltOnDeliveryFailure_NeverOverwritesAnExistingLatch(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	WriteSessionHalt(nopLogger(), "run-preexisting", client.Evaluation{Reason: "org kill switch", PolicyID: "p-1"})

	ev := devEventFor("run-preexisting", client.EventToolCall)
	HaltOnDeliveryFailure(nopLogger(), ev, errors.New("boom"))

	info, halted := SessionHalted("run-preexisting")
	if !halted {
		t.Fatal("must still read halted")
	}
	if info.Reason != "org kill switch" || info.PolicyID != "p-1" {
		t.Errorf("the verdict latch was overwritten by a later delivery failure: %+v", info)
	}
	if info.Cause != "" {
		t.Errorf("Cause = %q, want empty (this latch's origin is a verdict, not a delivery failure)", info.Cause)
	}
}

// TestHaltOnDeliveryFailure_ConcurrentFailuresWriteExactlyOneLatch is R1's
// write-if-absent guarantee under real concurrency (run with -race): many
// goroutines racing to latch the SAME run, each with a DIFFERENT failure
// class, must leave exactly one latch, whichever cause got there first.
func TestHaltOnDeliveryFailure_ConcurrentFailuresWriteExactlyOneLatch(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	const runID = "run-concurrent"
	errs := []error{
		errors.New("network unreachable"),
		context.DeadlineExceeded,
		client.ErrUnbuildable,
		fmt.Errorf("%w", client.ErrDelivery),
	}
	classes := make(map[string]bool, len(errs))
	for _, e := range errs {
		classes[client.FailureClass(e)] = true
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		err := errs[i%len(errs)]
		wg.Add(1)
		go func(err error) {
			defer wg.Done()
			HaltOnDeliveryFailure(nopLogger(), devEventFor(runID, client.EventToolCall), err)
		}(err)
	}
	wg.Wait()

	info, halted := SessionHalted(runID)
	if !halted {
		t.Fatal("the run must be latched")
	}
	if !classes[info.Cause] {
		t.Errorf("latch Cause = %q, want one of the attempted classes %v", info.Cause, classes)
	}
}

// TestNewEngine_OnFailureLedgersAndLatches is R3's wiring: Engine's default
// Spool.OnFailure (set once in NewEngine) both records the ledger line a
// drain has always written and, additively, the halt latch -- for any
// caller that never overrides Spool.OnFailure itself.
func TestNewEngine_OnFailureLedgersAndLatches(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	e := NewEngine(t.TempDir())
	failing := func(ctx context.Context, ev client.DevEvent) error { return errors.New("core refused it") }

	ev := client.DevEvent{EventID: "e1", EventType: client.EventToolCall, SessionID: "sess-engine-default"}
	if err := e.Spool.Append(ev); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := e.DrainSession(context.Background(), "sess-engine-default", emitterFunc(failing), DrainOptions{Mode: Block, AttemptTimeout: 2 * time.Second}); err != nil {
		t.Fatalf("DrainSession: %v", err)
	}

	if got := e.Spool.DiscardedCount(); got != 1 {
		t.Errorf("DiscardedCount = %d, want 1 (the ledger line every drain has always written)", got)
	}
	if _, halted := SessionHalted("sess-engine-default"); !halted {
		t.Error("the failed drain must also latch the run (additive to the ledger line)")
	}
}

// emitterFunc adapts a plain func to the Emitter interface DrainSession
// consumes (through Engine.emitFunc -> Deliver), for a test that only cares
// about the failure path, not a real verdict.
type emitterFunc func(context.Context, client.DevEvent) error

func (f emitterFunc) Emit(ctx context.Context, ev client.DevEvent) (client.Evaluation, error) {
	return client.Evaluation{}, f(ctx, ev)
}
