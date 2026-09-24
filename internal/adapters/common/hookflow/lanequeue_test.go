package hookflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

func laneEngine(t *testing.T) *Engine {
	t.Helper()
	return NewEngine(t.TempDir())
}

// waitUntil polls cond every 5ms until it reports true or timeout elapses,
// failing the test on timeout: every assertion here waits for an
// asynchronous Kick-spawned drain, never a fixed sleep.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition never became true within %s", timeout)
}

// TestLaneQueueDeliversInAppendOrder pins the core append-then-drain
// invariant: three events appended in sequence -- whether or not a drain is
// already running when a later one lands -- are delivered in that same
// order, never reordered by the asynchronous Kick.
func TestLaneQueueDeliversInAppendOrder(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	em := &countingEmitter{}
	q := NewLaneQueue(laneEngine(t), em, t.Logf)

	for _, id := range []string{"e1", "e2", "e3"} {
		if !q.Deliver(context.Background(), sessEv("sess-order", id)) {
			t.Fatalf("Deliver(%s) = false, want true", id)
		}
	}

	waitUntil(t, 2*time.Second, func() bool { return len(em.delivered()) == 3 })
	got := em.delivered()
	want := []string{"e1", "e2", "e3"}
	for i, id := range want {
		if got[i] != id {
			t.Fatalf("delivered order = %v, want %v", got, want)
		}
	}
	if q.Dropped() != 0 {
		t.Errorf("Dropped() = %d, want 0 for an all-accepted run", q.Dropped())
	}
}

// TestLaneQueueSingleFlightNoConcurrentDrainOfOneSession is the -race-checked
// single-flight property: 100 concurrent Delivers for the SAME session must
// never let two drain passes invoke Emit for that session at once (the
// stripe lock already forbids it structurally; this additionally proves
// Kick's own dirty-flag bookkeeping never loses or duplicates a line), and
// every one of the 100 distinct events is delivered exactly once.
func TestLaneQueueSingleFlightNoConcurrentDrainOfOneSession(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	const n = 100
	track := &concurrencyTrackingEmitter{}
	q := NewLaneQueue(laneEngine(t), track, t.Logf)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ev := sessEv("sess-concurrent", eventIDFor(i))
			q.Deliver(context.Background(), ev)
		}(i)
	}
	wg.Wait()

	waitUntil(t, 5*time.Second, func() bool { return track.delivered() == n })

	if got := track.maxConcurrent("sess-concurrent"); got > 1 {
		t.Errorf("max concurrent Emit calls for one session = %d, want <=1 (single-flight broken)", got)
	}
	if got := track.delivered(); got != n {
		t.Errorf("delivered = %d, want %d (no duplicate, no loss)", got, n)
	}
}

// TestLaneQueueUnacceptedEventLatchesRunThenNextEventStillAttemptedOnce
// pins: an event core refuses gets exactly one attempt and halts the run
// (HaltOnDeliveryFailure, wired through Engine's own Spool.OnFailure); a
// LATER event of the SAME session is still attempted -- once, never skipped
// or retried -- exactly like a halted run's remaining hook events.
func TestLaneQueueUnacceptedEventLatchesRunThenNextEventStillAttemptedOnce(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	em := &countingEmitter{script: map[string]error{"e-refused": errors.New("503 service unavailable")}}
	q := NewLaneQueue(laneEngine(t), em, t.Logf)

	if !q.Deliver(context.Background(), sessEv("sess-halt", "e-refused")) {
		t.Fatal("Deliver must accept the append even though delivery will fail")
	}

	waitUntil(t, 2*time.Second, func() bool {
		_, halted := SessionHalted("sess-halt")
		return halted
	})
	if got := em.attemptsFor("e-refused"); got != 1 {
		t.Errorf("attempts for the refused event = %d, want exactly 1", got)
	}
	if got := q.Dropped(); got != 1 {
		t.Errorf("Dropped() = %d, want 1 after one unaccepted event", got)
	}

	if !q.Deliver(context.Background(), sessEv("sess-halt", "e-after")) {
		t.Fatal("Deliver for a later event of the same (now-halted) run must still be accepted")
	}
	waitUntil(t, 2*time.Second, func() bool { return em.attemptsFor("e-after") == 1 })
	if got := em.attemptsFor("e-after"); got != 1 {
		t.Errorf("attempts for the later event = %d, want exactly 1 (never skipped, never retried)", got)
	}

	// An unrelated session is untouched.
	if _, halted := SessionHalted("sess-unrelated"); halted {
		t.Error("an unrelated session must not be latched")
	}
}

// TestLaneQueueDeliverAppendFailureCountsAndLatches pins Deliver's own
// append-failure branch: a spool write that never even lands counts the
// same as an unaccepted event and halts the run, since there is nothing
// left to retry it with.
func TestLaneQueueDeliverAppendFailureCountsAndLatches(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	// A spool Dir that is actually a FILE, not a directory, makes every
	// Append fail at MkdirAll.
	dir := t.TempDir()
	blockerFile := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blockerFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(filepath.Join(blockerFile, "spool"))
	q := NewLaneQueue(engine, &countingEmitter{}, t.Logf)

	ev := sessEv("sess-append-fail", "e1")
	if q.Deliver(context.Background(), ev) {
		t.Fatal("Deliver must report false when the append itself fails")
	}
	if got := q.Dropped(); got != 1 {
		t.Errorf("Dropped() = %d, want 1 for an append failure", got)
	}
	if _, halted := SessionHalted("sess-append-fail"); !halted {
		t.Error("an append failure must latch the run: nothing else will ever retry it")
	}
}

// TestLaneQueueCloseWaitsForAnInFlightDrain proves Close blocks until a
// drain in progress finishes (reporting 0 abandoned) when its own ctx allows
// enough time -- mirroring DeliverPool's own precedent
// (TestDeliverPoolCloseWaitsForInFlightDeliveries).
func TestLaneQueueCloseWaitsForAnInFlightDrain(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	em := blockingEmitterFunc(func(ctx context.Context, ev client.DevEvent) (client.Evaluation, error) {
		startedOnce.Do(func() { close(started) })
		<-release
		return client.Evaluation{}, nil
	})
	q := NewLaneQueue(laneEngine(t), em, t.Logf)

	if !q.Deliver(context.Background(), sessEv("sess-close-wait", "e1")) {
		t.Fatal("Deliver must accept")
	}
	<-started // the drain goroutine is now inside its Emit call, not merely queued

	closeDone := make(chan int, 1)
	go func() { closeDone <- q.Close(context.Background()) }()

	select {
	case <-closeDone:
		t.Fatal("Close returned before the in-flight delivery finished")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case abandoned := <-closeDone:
		if abandoned != 0 {
			t.Errorf("Close reported %d abandoned, want 0: the delivery finished before the deadline", abandoned)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close never returned after the delivery finished")
	}
}

// TestLaneQueueCloseAbandonsPastItsOwnDeadline is the other half: a short
// ctx must bound Close even while a delivery is still running, and report
// that count -- but must NOT add it to Dropped() (see the doc comment on
// Close: the record survives on disk and is counted once, later, by
// whichever drainer reclaims it).
func TestLaneQueueCloseAbandonsPastItsOwnDeadline(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release) // let the goroutine exit after the test, not leak it
	var startedOnce sync.Once
	em := blockingEmitterFunc(func(ctx context.Context, ev client.DevEvent) (client.Evaluation, error) {
		startedOnce.Do(func() { close(started) })
		<-release
		return client.Evaluation{}, nil
	})
	q := NewLaneQueue(laneEngine(t), em, t.Logf)

	if !q.Deliver(context.Background(), sessEv("sess-close-abandon", "e1")) {
		t.Fatal("Deliver must accept")
	}
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	abandoned := q.Close(ctx)
	if abandoned != 1 {
		t.Fatalf("Close reported %d abandoned, want 1", abandoned)
	}
	if got := q.Dropped(); got != 0 {
		t.Errorf("Dropped() = %d after an abandoned-but-not-lost drain, want 0", got)
	}
}

// blockingEmitterFunc adapts a func to the Emitter interface for a test that
// needs to synchronize on the exact moment Emit is entered.
type blockingEmitterFunc func(ctx context.Context, ev client.DevEvent) (client.Evaluation, error)

func (f blockingEmitterFunc) Emit(ctx context.Context, ev client.DevEvent) (client.Evaluation, error) {
	return f(ctx, ev)
}

// concurrencyTrackingEmitter records, per session, the maximum number of
// Emit calls ever in flight at once, and the total delivered count -- what
// TestLaneQueueSingleFlightNoConcurrentDrainOfOneSession needs to prove the
// single-flight guard actually holds under concurrent Delivers, not merely
// that the stripe lock alone would have prevented an overlap.
type concurrencyTrackingEmitter struct {
	mu      sync.Mutex
	current map[string]int
	maxSeen map[string]int
	total   int64
}

func (e *concurrencyTrackingEmitter) Emit(_ context.Context, ev client.DevEvent) (client.Evaluation, error) {
	e.mu.Lock()
	if e.current == nil {
		e.current = map[string]int{}
		e.maxSeen = map[string]int{}
	}
	e.current[ev.SessionID]++
	if e.current[ev.SessionID] > e.maxSeen[ev.SessionID] {
		e.maxSeen[ev.SessionID] = e.current[ev.SessionID]
	}
	e.mu.Unlock()

	time.Sleep(time.Millisecond) // give a broken guard a real chance to overlap

	e.mu.Lock()
	e.current[ev.SessionID]--
	e.mu.Unlock()

	atomic.AddInt64(&e.total, 1)
	return client.Evaluation{}, nil
}

func (e *concurrencyTrackingEmitter) maxConcurrent(sessionID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.maxSeen[sessionID]
}

func (e *concurrencyTrackingEmitter) delivered() int64 { return atomic.LoadInt64(&e.total) }

func eventIDFor(i int) string {
	const hex = "0123456789abcdef"
	b := make([]byte, 0, 8)
	b = append(b, 'e')
	for _, shift := range []uint{12, 8, 4, 0} {
		b = append(b, hex[(i>>shift)&0xf])
	}
	return string(b)
}

// TestLaneQueueKickCloseRace hammers Kick (via Deliver) against Close from
// many goroutines at once: wg.Add(1) and the closed check/set must be one
// atomic step (both under q.mu), or sync.WaitGroup's own contract ("calls
// with a positive delta... must happen before a Wait") is violated the
// moment a Kick's wg.Add lands after Close has already started (or
// finished) wg.Wait -- caught by `go test -race` as a WaitGroup misuse
// panic, not merely a logical inconsistency. Passing (no panic, no hang) IS
// the assertion; run with -race.
func TestLaneQueueKickCloseRace(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	q := NewLaneQueue(laneEngine(t), &countingEmitter{}, t.Logf)

	const goroutines = 20
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			session := "sess-" + strconv.Itoa(i%4)
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				q.Deliver(context.Background(), sessEv(session, "e-"+strconv.Itoa(i)+"-"+strconv.Itoa(j)))
			}
		}(i)
	}

	// Let a real burst of Kicks land before racing Close against them.
	time.Sleep(5 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	q.Close(ctx)
	cancel()

	close(stop)
	wg.Wait()
}
