package hookflow

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// TestDeliverPoolSubmitNeverBlocksTheCaller pins the request-goroutine-never-
// blocks property: Submit must return well before a slow delivery finishes.
func TestDeliverPoolSubmitNeverBlocksTheCaller(t *testing.T) {
	release := make(chan struct{})
	p := NewDeliverPool(1, time.Minute, func(ctx context.Context, ev client.DevEvent) error {
		<-release
		return nil
	})
	defer close(release)

	done := make(chan struct{})
	go func() {
		p.Submit(client.DevEvent{EventID: "e1"})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit blocked on a slow delivery; it must return immediately")
	}
}

// TestDeliverPoolSaturationDropsAndCounts asserts pool saturation drops a
// record rather than queuing it, and the drop is counted -- decision-
// 260922-1445's accepted risk, not silent loss.
func TestDeliverPoolSaturationDropsAndCounts(t *testing.T) {
	release := make(chan struct{})
	var delivered int32
	p := NewDeliverPool(1, time.Minute, func(ctx context.Context, ev client.DevEvent) error {
		atomic.AddInt32(&delivered, 1)
		<-release
		return nil
	})
	defer close(release)

	if ok := p.Submit(client.DevEvent{EventID: "first"}); !ok {
		t.Fatal("first Submit into an empty pool must be accepted")
	}
	// The pool has size 1 and the first delivery is parked on release, so a
	// second submission must be dropped rather than queued.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&delivered) == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if ok := p.Submit(client.DevEvent{EventID: "second"}); ok {
		t.Fatal("Submit into a saturated pool must be dropped, not queued")
	}
	if got := p.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d; want 1", got)
	}
}

// TestDeliverPoolOneAttemptPerRecord: every accepted record is delivered
// exactly once, never retried.
func TestDeliverPoolOneAttemptPerRecord(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	p := NewDeliverPool(10, time.Second, func(ctx context.Context, ev client.DevEvent) error {
		mu.Lock()
		seen[ev.EventID]++
		mu.Unlock()
		return nil
	})
	for i := 0; i < 10; i++ {
		if !p.Submit(client.DevEvent{EventID: "evt"}) {
			t.Fatalf("Submit %d dropped in a pool large enough to accept every one of them", i)
		}
	}
	// Give the pool's goroutines a moment to run; there is no queue to drain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := seen["evt"]
		mu.Unlock()
		if n == 10 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["evt"] != 10 {
		t.Fatalf("delivered %d of 10 accepted records", seen["evt"])
	}
}

// stubEmitter is a minimal Emitter for TestDeliverOrdering.
type stubEmitter struct {
	fail func(ev client.DevEvent) bool
}

func (s stubEmitter) Emit(ctx context.Context, ev client.DevEvent) (client.Evaluation, error) {
	if s.fail != nil && s.fail(ev) {
		return client.Evaluation{}, context.DeadlineExceeded
	}
	return client.Evaluation{}, nil
}

// TestDeliverOrderingSubmitsStartedFirst pins ordering at the CALLER: a
// caller submits Started before Completed and both land in the pool. That
// the pool then delivers them in that order is
// TestDeliverPoolDeliversOneSessionInSubmitOrder's claim.
func TestDeliverOrderingSubmitsStartedFirst(t *testing.T) {
	var mu sync.Mutex
	var delivered []string
	p := NewDeliverPool(4, time.Second, func(ctx context.Context, ev client.DevEvent) error {
		_, _ = Deliver(ctx, stubEmitter{}, nil, ev, nil)
		mu.Lock()
		delivered = append(delivered, string(ev.EventType))
		mu.Unlock()
		return nil
	})

	started := client.DevEvent{EventID: "e1", EventType: client.EventTurnStarted}
	completed := client.DevEvent{EventID: "e1", EventType: client.EventTurnCompleted}

	// A caller submits Started first; only submits Completed if Started was
	// accepted -- this ordering rule is the caller's, not the pool's.
	if ok := p.Submit(started); !ok {
		t.Fatal("Started was refused by an otherwise-empty pool")
	}
	if ok := p.Submit(completed); !ok {
		t.Fatal("Completed was refused right after Started was accepted")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(delivered)
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 2 {
		t.Fatalf("delivered %v; want both halves", delivered)
	}
}

// TestDeliverOrderingSkipsCompletedAfterARefusedStarted is the load-bearing
// half, pinned at the pool's own Submit return, which is the signal a caller
// (gatewayemit/telemetryemit's Emit) acts on to skip Completed outright --
// see TestAFailedAppendAbandonsTheActivityRatherThanOrphaningAHalf in
// gatewayemit and its telemetryemit counterpart for the caller-side contract.
func TestDeliverOrderingSkipsCompletedAfterARefusedStarted(t *testing.T) {
	release := make(chan struct{})
	p := NewDeliverPool(1, time.Minute, func(ctx context.Context, ev client.DevEvent) error {
		<-release
		return nil
	})
	defer close(release)

	// Saturate the pool with an unrelated in-flight delivery.
	p.Submit(client.DevEvent{EventID: "occupier"})

	if ok := p.Submit(client.DevEvent{EventID: "e1", EventType: client.EventTurnStarted}); ok {
		t.Fatal("expected the saturated pool to refuse Started, which is the caller's signal to skip Completed")
	}
}

// TestDeliverPoolCloseWaitsForInFlightDeliveries is the shutdown-drain fix: a
// daemon that calls Close before exiting must not have os.Exit kill an
// otherwise-healthy in-flight delivery mid-goroutine. Close must block until
// the delivery actually finishes, not merely until Submit returned.
func TestDeliverPoolCloseWaitsForInFlightDeliveries(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var finished int32
	p := NewDeliverPool(4, time.Minute, func(ctx context.Context, ev client.DevEvent) error {
		close(started)
		<-release
		atomic.AddInt32(&finished, 1)
		return nil
	})

	if ok := p.Submit(client.DevEvent{EventID: "in-flight"}); !ok {
		t.Fatal("Submit into an empty pool must be accepted")
	}
	<-started // the delivery goroutine is now running, not merely queued

	closeDone := make(chan int)
	go func() { closeDone <- p.Close(context.Background()) }()

	// Close must not have returned yet: the delivery is still parked on
	// release. A flaky sleep-based check would be wrong in both directions,
	// so assert the negative with a short timeout instead.
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
	if atomic.LoadInt32(&finished) != 1 {
		t.Fatal("the delivery never actually ran to completion")
	}
}

// TestDeliverPoolCloseAbandonsAndCountsWhatMissesTheDeadline is the other
// half: a delivery still running when Close's deadline expires is abandoned
// (the process is about to exit regardless) and counted as dropped, so
// `doctor`'s one counter covers this loss mode too, not just saturation.
func TestDeliverPoolCloseAbandonsAndCountsWhatMissesTheDeadline(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // let the goroutine exit after the test, not leak it
	p := NewDeliverPool(4, time.Minute, func(ctx context.Context, ev client.DevEvent) error {
		<-release
		return nil
	})
	if ok := p.Submit(client.DevEvent{EventID: "stuck"}); !ok {
		t.Fatal("Submit into an empty pool must be accepted")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	abandoned := p.Close(ctx)
	if abandoned != 1 {
		t.Fatalf("Close reported %d abandoned, want 1", abandoned)
	}
	if got := p.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d after an abandoned in-flight delivery, want 1", got)
	}
}

// TestDeliverPoolCloseRefusesNewSubmissions: once shutdown has started,
// Submit must refuse rather than accept work Close is not waiting for.
func TestDeliverPoolCloseRefusesNewSubmissions(t *testing.T) {
	p := NewDeliverPool(4, time.Minute, func(ctx context.Context, ev client.DevEvent) error { return nil })
	if abandoned := p.Close(context.Background()); abandoned != 0 {
		t.Fatalf("Close on an idle pool abandoned %d, want 0", abandoned)
	}
	if ok := p.Submit(client.DevEvent{EventID: "too-late"}); ok {
		t.Fatal("Submit after Close must be refused")
	}
	if got := p.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d for a post-Close Submit, want 1", got)
	}
}

// TestDeliverRecordsAdvisory pins Deliver's contract: it calls Emit, then
// records to Advisory when one is supplied, and never panics when Advisory is
// nil (a lane daemon may run without one).
func TestDeliverRecordsAdvisory(t *testing.T) {
	dir := t.TempDir() + "/advisory.jsonl"
	adv := &Advisory{Path: dir}
	ev := client.DevEvent{EventID: "e1", SessionID: "s1"}
	if _, err := Deliver(context.Background(), stubEmitter{}, adv, ev, nil); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if _, err := Deliver(context.Background(), stubEmitter{}, nil, ev, nil); err != nil {
		t.Fatalf("Deliver with nil advisory: %v", err)
	}
}

// TestDeliverPoolDeliversOneSessionInSubmitOrder pins per-session ordering:
// a session's second record must not start until its first has finished, so
// a chat's WorkflowStarted reaches core before the activity halves that need
// its session to exist -- while a DIFFERENT session is not held up by it.
func TestDeliverPoolDeliversOneSessionInSubmitOrder(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan string, 8)
	p := NewDeliverPool(8, time.Minute, func(ctx context.Context, ev client.DevEvent) error {
		entered <- ev.EventID
		if ev.EventID == "a1" {
			<-release
		}
		return nil
	})
	for _, id := range []string{"a1", "a2", "a3"} {
		if !p.Submit(client.DevEvent{EventID: id, SessionID: "chat-a"}) {
			t.Fatalf("Submit(%s) refused by a pool with room", id)
		}
	}
	if !p.Submit(client.DevEvent{EventID: "b1", SessionID: "chat-b"}) {
		t.Fatal("Submit(b1) refused by a pool with room")
	}

	got := map[string]bool{}
	timeout := time.After(time.Second)
	for !(got["a1"] && got["b1"]) {
		select {
		case id := <-entered:
			if id == "a2" || id == "a3" {
				t.Fatalf("%s started while a1 of the same session was still in flight", id)
			}
			got[id] = true
		case <-timeout:
			t.Fatalf("entered %v; want a1 and b1 running concurrently", got)
		}
	}
	select {
	case id := <-entered:
		t.Fatalf("%s started while a1 of the same session was still in flight", id)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	for _, want := range []string{"a2", "a3"} {
		select {
		case id := <-entered:
			if id != want {
				t.Fatalf("delivered %s, want %s: a session's records go in submit order", id, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s was never delivered", want)
		}
	}
}

// TestDeliverPoolStopsASessionAtItsFirstUnacceptedRecord: a record core did
// not accept ends its session's delivery. What was queued behind it is
// dropped and counted, never sent -- a Completed after a refused Started is
// the orphan half -- and a different session is unaffected.
func TestDeliverPoolStopsASessionAtItsFirstUnacceptedRecord(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	release := make(chan struct{})
	p := NewDeliverPool(8, time.Minute, func(ctx context.Context, ev client.DevEvent) error {
		mu.Lock()
		sent = append(sent, ev.EventID)
		mu.Unlock()
		if ev.EventID == "a1" {
			<-release
			return errors.New("core did not accept")
		}
		return nil
	})
	for _, ev := range []client.DevEvent{
		{EventID: "a1", SessionID: "chat-a"},
		{EventID: "a2", SessionID: "chat-a"},
		{EventID: "a3", SessionID: "chat-a"},
		{EventID: "b1", SessionID: "chat-b"},
	} {
		if !p.Submit(ev) {
			t.Fatalf("Submit(%s) refused by a pool with room", ev.EventID)
		}
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if abandoned := p.Close(ctx); abandoned != 0 {
		t.Fatalf("Close abandoned %d, want 0", abandoned)
	}

	mu.Lock()
	defer mu.Unlock()
	got := map[string]bool{}
	for _, id := range sent {
		got[id] = true
	}
	if got["a2"] || got["a3"] {
		t.Fatalf("sent %v; nothing of chat-a may follow its refused a1", sent)
	}
	if !got["a1"] || !got["b1"] {
		t.Fatalf("sent %v; want a1 attempted and chat-b's b1 delivered", sent)
	}
	if n := p.Dropped(); n != 2 {
		t.Fatalf("Dropped() = %d, want 2 (a2, a3)", n)
	}
}

// TestDeliverPoolCloseAbandonsQueuedRecords: at Close's deadline a record
// still queued behind its session's in-flight one is taken out, never
// attempted, counted, and handed to OnAbandon; the in-flight one is counted
// only.
func TestDeliverPoolCloseAbandonsQueuedRecords(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var attempts int32
	p := NewDeliverPool(8, time.Minute, func(ctx context.Context, ev client.DevEvent) error {
		atomic.AddInt32(&attempts, 1)
		<-release
		return nil
	})
	var mu sync.Mutex
	var abandonedIDs []string
	p.OnAbandon = func(ev client.DevEvent) {
		mu.Lock()
		abandonedIDs = append(abandonedIDs, ev.EventID)
		mu.Unlock()
	}
	for _, id := range []string{"a1", "a2", "a3"} {
		if !p.Submit(client.DevEvent{EventID: id, SessionID: "chat-a"}) {
			t.Fatalf("Submit(%s) refused by a pool with room", id)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if abandoned := p.Close(ctx); abandoned != 3 {
		t.Fatalf("Close reported %d abandoned, want 3 (one in flight, two queued)", abandoned)
	}
	if n := p.Dropped(); n != 3 {
		t.Fatalf("Dropped() = %d, want 3", n)
	}
	if n := atomic.LoadInt32(&attempts); n != 1 {
		t.Fatalf("%d attempts made, want 1: a queued record must not start", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(abandonedIDs) != 2 || abandonedIDs[0] != "a2" || abandonedIDs[1] != "a3" {
		t.Fatalf("OnAbandon saw %v, want [a2 a3]", abandonedIDs)
	}
}
