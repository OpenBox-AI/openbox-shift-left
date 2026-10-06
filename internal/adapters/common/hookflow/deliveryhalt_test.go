package hookflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

func devEventFor(sessionID string, eventType client.EventType) client.DevEvent {
	return client.DevEvent{EventID: "e-" + sessionID, EventType: eventType, SessionID: sessionID}
}

// TestRecordDeliveryFailure_NeverLatchesTheRun: an unaccepted event is a
// finding, not a latch. Every failure class this exercises must leave the
// run entirely unhalted.
func TestRecordDeliveryFailure_NeverLatchesTheRun(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	ev := devEventFor("run-explicit", client.EventToolCall)
	err := fmt.Errorf("%w: dial tcp: connection refused", client.ErrDelivery)

	RecordDeliveryFailure(nopLogger(), ev, err)

	if _, halted := SessionHalted("run-explicit"); halted {
		t.Fatal("a delivery failure must never latch the run")
	}
}

// TestRecordDeliveryFailure_OrphanedDrainNeverLatchesEither pins the same
// non-latching contract for a reclaimed crash orphan, whose outcome cannot
// be proven at all.
func TestRecordDeliveryFailure_OrphanedDrainNeverLatchesEither(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	ev := devEventFor("run-orphan", client.EventSessionEnded)
	RecordDeliveryFailure(nopLogger(), ev, errOrphanedDrain)

	if _, halted := SessionHalted("run-orphan"); halted {
		t.Error("a reclaimed orphan must not latch the run either")
	}
}

// TestRecordDeliveryFailure_NeverDisturbsAPreexistingRealHaltLatch: a live
// HALT verdict (WriteSessionHalt, an entirely different origin) already
// latched this run; a LATER delivery failure for the same run must leave
// that latch exactly as it was -- RecordDeliveryFailure writes nothing at
// all, so there is nothing for it to disturb.
func TestRecordDeliveryFailure_NeverDisturbsAPreexistingRealHaltLatch(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	WriteSessionHalt(nopLogger(), "run-preexisting", client.Evaluation{Reason: "org kill switch", PolicyID: "p-1"})

	ev := devEventFor("run-preexisting", client.EventToolCall)
	RecordDeliveryFailure(nopLogger(), ev, errors.New("boom"))

	info, halted := SessionHalted("run-preexisting")
	if !halted {
		t.Fatal("must still read halted: the real verdict latch")
	}
	if info.Reason != "org kill switch" || info.PolicyID != "p-1" {
		t.Errorf("the verdict latch was disturbed by a later delivery failure: %+v", info)
	}
	if info.Cause != "" {
		t.Errorf("Cause = %q, want empty (this latch's origin is a verdict, not a delivery failure)", info.Cause)
	}
}

// TestRecordDeliveryFailure_ConcurrentFailuresNeverLatch is the write-if-
// absent test's negative-space successor under real concurrency (run with
// -race): many goroutines racing to record the SAME run's failures, each
// with a DIFFERENT failure class, must leave the run entirely unlatched.
func TestRecordDeliveryFailure_ConcurrentFailuresNeverLatch(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	const runID = "run-concurrent"
	errs := []error{
		errors.New("network unreachable"),
		context.DeadlineExceeded,
		client.ErrUnbuildable,
		fmt.Errorf("%w", client.ErrDelivery),
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		err := errs[i%len(errs)]
		wg.Add(1)
		go func(err error) {
			defer wg.Done()
			RecordDeliveryFailure(nopLogger(), devEventFor(runID, client.EventToolCall), err)
		}(err)
	}
	wg.Wait()

	if _, halted := SessionHalted(runID); halted {
		t.Error("no combination of concurrent delivery failures may latch the run")
	}
}

// TestNewEngine_OnFailureLedgersButNeverLatches is the wiring: Engine's
// default Spool.OnFailure (set once in NewEngine) records the ledger line a
// drain has always written, but never latches the run, for any caller
// that never overrides Spool.OnFailure itself.
func TestNewEngine_OnFailureLedgersButNeverLatches(t *testing.T) {
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
	if _, halted := SessionHalted("sess-engine-default"); halted {
		t.Error("a failed drain must never latch the run")
	}
}

// TestNewEngine_TimeoutClassNeverLatchesAfterOneRetry proves the timeout
// failure class through the SAME wiring the real 30s flusher/lane-drain
// path uses (Engine.NewEngine's own default Spool.OnFailure ->
// RecordDeliveryFailure), without waiting out the real 30s bound: a short
// AttemptTimeout stands in for it. RequeueUnanswered is left false (the
// zero value, matching every non-inline drainer -- the detached flusher,
// the sweep, a lane daemon's own queue), so an attempt whose own ctx expires
// before the emitter returns is scored as an ordinary failure, not
// requeued.
func TestNewEngine_TimeoutClassNeverLatchesAfterOneRetry(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	e := NewEngine(t.TempDir())
	const sessionID = "sess-timeout-class"
	ev := client.DevEvent{EventID: "e1", EventType: client.EventToolCall, SessionID: sessionID}
	if err := e.Spool.Append(ev); err != nil {
		t.Fatalf("append: %v", err)
	}

	attempts := 0
	blockPastDeadline := emitterFunc(func(ctx context.Context, _ client.DevEvent) error {
		attempts++
		<-ctx.Done()
		// %w on ctx.Err() too (not %s): client.FailureClass classifies
		// "timeout" via errors.Is(err, context.DeadlineExceeded), which a
		// merely-stringified ctx.Err() could never satisfy.
		return fmt.Errorf("%w: %w", client.ErrDelivery, ctx.Err())
	})

	const attemptTimeout = 50 * time.Millisecond
	n, err := e.DrainSession(context.Background(), sessionID, blockPastDeadline, DrainOptions{
		Mode: Block, AttemptTimeout: attemptTimeout,
	})
	if err != nil {
		t.Fatalf("DrainSession: %v", err)
	}
	if n != 0 {
		t.Errorf("delivered = %d, want 0 (the only line timed out)", n)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want exactly 2 (one attempt, one retry)", attempts)
	}
	if _, halted := SessionHalted(sessionID); halted {
		t.Error("a timed-out delivery must never latch the run")
	}
	if got := e.Spool.DiscardedCount(); got != 1 {
		t.Errorf("DiscardedCount = %d, want 1 (the ledger line every drain has always written)", got)
	}
}

// emitterFunc adapts a plain func to the Emitter interface DrainSession
// consumes (through Engine.emitFunc -> Deliver), for a test that only cares
// about the failure path, not a real verdict.
type emitterFunc func(context.Context, client.DevEvent) error

func (f emitterFunc) Emit(ctx context.Context, ev client.DevEvent) (client.Evaluation, error) {
	return client.Evaluation{}, f(ctx, ev)
}
