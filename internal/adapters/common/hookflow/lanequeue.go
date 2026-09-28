package hookflow

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// LanePassBudget bounds one LaneQueue drain pass (the ctx DrainSession itself
// receives): generous enough to cover more than one DeliveryAttemptTimeout-
// bounded attempt when a backlog has piled up, so a busy session does not
// need a fresh Kick per line to finish draining. It is not a target duration:
// an ordinary pass returns as soon as its session's queue is empty, long
// before this elapses.
const LanePassBudget = 60 * time.Second

// LaneQueue is what a lane daemon (openbox transport, openbox telemetry)
// appends its OWN model-call records to instead of Spool.Append +
// RealtimeTrigger.Maybe (the hooks lane's own nudge) or a DeliverPool
// submission (this queue's in-process, spool-backed replacement for a lane's
// own records): append, then drain in-process through THIS daemon's own
// pre-resolved, per-provider signing client (Client) -- a lane daemon
// already holds the right client with a memory token; spawning a flusher to
// re-resolve it would buy nothing.
//
// Engine's own spool directory is shared with that provider's hook events
// (cc-spool, codex-spool): a lane record appended here interleaves, in
// append order, with whatever a hook process already queued for the same
// session, so one drain delivers both in the order they were produced.
// Construct one LaneQueue per provider, each over its own Engine (its own
// spool dir) and its own Client (that provider's own signing identity) --
// never share an Engine between two queues, or one provider's own delivery
// count / OnFailure wiring would double-count the other's.
type LaneQueue struct {
	Engine *Engine
	// Client signs and delivers every record this queue drains: the lane
	// daemon's own pre-resolved identity for the ONE provider this queue
	// belongs to.
	Client Emitter
	// Log reports what a drain pass could not do. Nil ⇒ silent.
	Log func(format string, args ...any)

	// mu guards running, dirty, closed AND every wg.Add(1) this type ever
	// makes: Close's own "may I stop accepting new work" decision and Kick's
	// own "am I allowed to start more" decision must be the SAME atomic step
	// as the wg.Add itself, or a Kick in flight when Close runs can add to wg
	// AFTER Close has already started (or finished) wg.Wait -- an unawaited
	// drain, and sync.WaitGroup's own contract ("Note that calls with a
	// positive delta... must happen before a Wait") makes that a race the
	// detector can catch, not merely a cosmetic one.
	mu      sync.Mutex
	running map[string]bool
	dirty   map[string]bool
	closed  bool
	wg      sync.WaitGroup

	dropped uint64
}

// NewLaneQueue builds a LaneQueue draining through engine using cl. It wraps
// engine's own Spool.OnFailure (already wired by NewEngine to the discard
// ledger line plus RecordDeliveryFailure) so this queue's own Dropped()
// count -- what a lane daemon's status file discloses to `doctor` -- covers
// every event a drain attempted and core did not accept, not only an append
// failure counted directly in Deliver. engine must belong to this queue
// alone (see the type doc).
func NewLaneQueue(engine *Engine, cl Emitter, logf func(format string, args ...any)) *LaneQueue {
	q := &LaneQueue{Engine: engine, Client: cl, Log: logf}
	prevFailure := engine.Spool.OnFailure
	engine.Spool.OnFailure = func(ev client.DevEvent, err error) {
		atomic.AddUint64(&q.dropped, 1)
		if prevFailure != nil {
			prevFailure(ev, err)
			return
		}
		// Defensive only: every production caller builds engine via NewEngine,
		// which always wires a non-nil OnFailure (ledger + finding) before a
		// LaneQueue is ever built over it.
		engine.speaking().defaultOnFailure(ev, err)
		RecordDeliveryFailure(log.New(logfWriter{logf: logf}, "", 0), ev, err)
	}
	return q
}

func (q *LaneQueue) logf(format string, args ...any) {
	if q.Log != nil {
		q.Log(format, args...)
	}
}

// Deliver appends ev to this queue's own spool -- raw (Spool.Append), never
// Engine.Record: Record's ThreadDuration bridges a hook's own Pre/PostToolUse
// pair across two processes, which a lane record (already a completed model
// call, produced by one process, never paired across two) has no use for.
// It then kicks that session's drain so the record is attempted without
// waiting for a hook of the same session to trigger one. An append failure
// means the record never reached even the spool it would have been
// delivered from -- counted (Dropped) and, since there is nothing else to
// retry it with, recorded as a delivery-failure finding exactly like an
// unaccepted event (RecordDeliveryFailure) -- never a run latch.
func (q *LaneQueue) Deliver(ctx context.Context, ev client.DevEvent) bool {
	if err := q.Engine.Spool.Append(ev); err != nil {
		atomic.AddUint64(&q.dropped, 1)
		q.logf("lanequeue: %s could not be spooled: %v", ev.EventID, err)
		q.Engine.Spool.recordDiscard(q.Engine.Spool.SessionPath(ev.SessionID), 1,
			"could not be spooled: "+err.Error())
		tracePoolDropSubmit(ev, "could_not_be_spooled")
		RecordDeliveryFailure(log.New(logfWriter{logf: q.logf}, "", 0), ev, err)
		return false
	}
	q.Kick(ev.SessionID)
	return true
}

// Kick starts (or, when one is already running, marks dirty) sessionID's
// drain: at most one drain goroutine per session at a time, so two Delivers
// for the same busy session never spawn a second drainer that would only
// block on the first one's own stripe lock for nothing. A Deliver landing
// while a drain is already in flight sets dirty instead, so the running
// drain loops once more after it finishes rather than the new line waiting
// for some LATER Kick to notice it -- every appended line still gets its one
// attempt this pass or the very next one, never silently deferred.
//
// wg.Add(1) happens INSIDE the same critical section as the closed check and
// the spawn decision, not after releasing q.mu: Close's own "stop accepting
// work" (setting closed) and its wg.Wait both serialize against this same
// mutex, so a Kick that is ever going to add to wg has always already done
// so by the time Close observes closed==true and starts waiting -- there is
// no window where Close's wg.Wait can return before a Kick it raced against
// has counted its own drain in.
func (q *LaneQueue) Kick(sessionID string) {
	q.mu.Lock()
	if q.closed {
		// The record just appended stays spooled -- not lost, not a failure --
		// for a later drain to find: the hooks lane's own periodic sweep shares
		// this exact directory, and a fresh inline SessionStart/SessionEnd
		// attempt or this daemon's own next start will drain it.
		q.mu.Unlock()
		return
	}
	if q.running == nil {
		q.running = map[string]bool{}
		q.dirty = map[string]bool{}
	}
	if q.running[sessionID] {
		q.dirty[sessionID] = true
		q.mu.Unlock()
		return
	}
	q.running[sessionID] = true
	q.wg.Add(1)
	q.mu.Unlock()

	go q.drain(sessionID)
}

func (q *LaneQueue) drain(sessionID string) {
	defer q.wg.Done()
	for {
		passStart := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), LanePassBudget)
		n, err := q.Engine.DrainSession(ctx, sessionID, q.Client, DrainOptions{
			Mode:           Block,
			AttemptTimeout: DeliveryAttemptTimeout,
		})
		cancel()
		traceLaneDrain(sessionID, passStart, n, err)
		if err != nil && !errors.Is(err, ErrSessionBusy) {
			q.logf("lanequeue: %s: drain pass ended early: %v", sessionID, err)
		}

		q.mu.Lock()
		if !q.closed && q.dirty[sessionID] {
			delete(q.dirty, sessionID)
			q.mu.Unlock()
			continue
		}
		delete(q.running, sessionID)
		delete(q.dirty, sessionID)
		q.mu.Unlock()
		return
	}
}

// Dropped is every record this queue could not deliver: an append that never
// reached the spool, plus every event a drain pass attempted and core did
// not accept. Both already recorded their own delivery-failure finding
// through RecordDeliveryFailure (never a run latch); this is `doctor`'s
// disclosure count, not a second decision.
func (q *LaneQueue) Dropped() uint64 { return atomic.LoadUint64(&q.dropped) }

// Close stops Kick starting any new drain and waits for every in-flight one
// to finish its current pass, up to ctx's own deadline -- a caller should
// give ctx a deadline no shorter than DeliveryAttemptTimeout, the same
// contract DeliverPool.Close documents, so an ordinary single in-flight
// attempt has room to finish. Whatever is still running when ctx is done is
// left running in the background (the caller is about to exit right behind
// its own return): that pass's own DrainSession call, killed mid-attempt by
// the exit, leaves an ordinary `.flushing.` orphan the next drainer reclaims
// and halts through -- the same crash-safety path every other drainer
// already relies on, never a resend. Deliberately NOT added to Dropped():
// unlike DeliverPool's own abandoned delivery (which really is lost), this
// record survives on disk and will be counted -- exactly once, by whichever
// drainer reclaims it -- so counting it here too would double it. Safe to
// call once; a later call is a no-op returning 0.
//
// Setting closed happens under the SAME q.mu a concurrent Kick's own
// wg.Add(1) is inside (see Kick's own doc): by the time this unlocks, every
// Kick that observed closed==false has already added to wg, and every one
// that runs afterward observes closed==true and adds nothing -- wg.Wait
// below can never race a wg.Add it has not already accounted for.
func (q *LaneQueue) Close(ctx context.Context) (abandoned int) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return 0
	}
	q.closed = true
	q.mu.Unlock()

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return 0
	case <-ctx.Done():
		q.mu.Lock()
		abandoned = len(q.running)
		q.mu.Unlock()
		return abandoned
	}
}
