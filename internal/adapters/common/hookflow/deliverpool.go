package hookflow

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// DefaultPoolSize and DefaultPoolTimeout size the transport lane's own
// claude.ai CHAT delivery pool (32 records outstanding, queued or in flight;
// 10s per emit, matching the gate's evaluateTimeout). A chat conversation has
// no session-scoped spool of its own (chats are not tool sessions; see
// sessionkey.IsChatKey) and no flusher behind it, so this pool's own
// backpressure is the only one this path has. A new conversation's first
// call alone holds three records at once (SessionStarted and both halves),
// delivered one after another, which is why the bound is not the 8 it was
// while every record ran on its own goroutine.
const (
	DefaultPoolSize    = 32
	DefaultPoolTimeout = 10 * time.Second
)

// DeliverPool bounds in-process delivery for a lane daemon that sends
// records itself instead of spooling them for a flusher: never the request
// goroutine, a per-emit timeout, and a dropped-record counter `doctor` can
// read.
//
// Records of one session (ev.SessionID) are delivered one at a time, in
// submit order, each only after the one before it finished: one drainer per
// session, the LaneQueue shape without the spool. Core files an activity
// event that reaches it before its session's WorkflowStarted with no
// session at all -- it looks the session up, finds none, and the session's
// own backfill has already run by the time the event is stored -- so
// concurrent delivery of a new conversation's first call orphaned whichever
// half lost the race. Different sessions still deliver concurrently.
//
// The first record deliver reports unaccepted stops its session: every
// record still queued behind it is dropped and counted, never sent, because
// a Completed sent after its Started was refused files exactly the orphan
// half this pool exists to prevent, and the refused record has already
// latched the run.
//
// Its one production caller today is the transport lane's own claude.ai
// CHAT records (a conversation has no tool session, so it has no per-session
// spool to append into and drain the way LaneQueue does). Every OTHER lane
// record -- a Claude Code or Codex model-call turn, on either the transport
// or telemetry lane -- goes through LaneQueue instead, which appends into
// and drains that provider's own session spool (the same one its hook
// events queue through), so a record interleaves with its session's hook
// events in append order rather than racing them through a separate,
// spool-less pool. It has nothing to do with the hooks lane's own spool,
// which keeps Spool, RealtimeTrigger and Sweeper unchanged.
type DeliverPool struct {
	size    int
	timeout time.Duration
	deliver func(ctx context.Context, ev client.DevEvent) error

	// OnAbandon is called for each record Submit accepted that Close's
	// deadline then abandoned before a single attempt was made: it never
	// reached core, so a caller latches it like any other unaccepted record.
	// A record abandoned mid-attempt is counted only, as it always was. Set
	// it before the first Submit; nil ⇒ count only.
	OnAbandon func(ev client.DevEvent)

	mu sync.Mutex
	// queues holds each session's records not yet started. A key is present
	// exactly while that session's drainer is running, which is what
	// decides whether Submit starts one.
	queues map[string][]client.DevEvent
	// outstanding is every accepted record not yet finished, queued or in
	// flight: what size bounds.
	outstanding int
	closed      bool
	wg          sync.WaitGroup
	dropped     uint64
}

// NewDeliverPool builds a pool of at most size outstanding records, each
// attempt bounded by timeout from when it starts, calling deliver for every
// accepted record; deliver reports whether core accepted it (nil) or not.
// size<=0 and timeout<=0 fall back to the defaults above.
func NewDeliverPool(size int, timeout time.Duration, deliver func(ctx context.Context, ev client.DevEvent) error) *DeliverPool {
	if size <= 0 {
		size = DefaultPoolSize
	}
	if timeout <= 0 {
		timeout = DefaultPoolTimeout
	}
	return &DeliverPool{
		size:    size,
		timeout: timeout,
		deliver: deliver,
		queues:  map[string][]client.DevEvent{},
	}
}

// Submit tries to accept ev for background delivery and returns immediately;
// it never blocks the caller on a network round trip. A full pool is a DROP,
// counted rather than queued past the bound: this accepts losing a record
// under load rather than growing an unbounded backlog in a process that has
// no spool to hold it. false means dropped.
//
// wg.Add runs under the same lock as the closed check, so Close can never
// start waiting before a drainer that raced it has been counted in (the
// reasoning LaneQueue.Kick documents).
func (p *DeliverPool) Submit(ev client.DevEvent) bool {
	p.mu.Lock()
	// After Close has started, admitting more work would be something Close
	// never waits for, and process exit right behind it would kill it
	// mid-goroutine exactly like the shutdown race Close exists to close.
	// Counted the same as a saturation drop: either way the record never
	// reached core.
	if p.closed || p.outstanding >= p.size {
		reason := "saturated"
		if p.closed {
			reason = "closed"
		}
		p.mu.Unlock()
		atomic.AddUint64(&p.dropped, 1)
		tracePoolDropSubmit(ev, reason)
		return false
	}
	p.outstanding++
	queued, running := p.queues[ev.SessionID]
	p.queues[ev.SessionID] = append(queued, ev)
	if !running {
		p.wg.Add(1)
		go p.drain(ev.SessionID)
	}
	p.mu.Unlock()
	return true
}

// drain delivers session's records until its queue is empty or one is not
// accepted, then releases the session so the next Submit starts a new
// drainer.
func (p *DeliverPool) drain(session string) {
	defer p.wg.Done()
	for {
		p.mu.Lock()
		queued := p.queues[session]
		if len(queued) == 0 {
			delete(p.queues, session)
			p.mu.Unlock()
			return
		}
		ev := queued[0]
		p.queues[session] = queued[1:]
		p.mu.Unlock()

		// The attempt's budget starts now, not at Submit: time spent queued
		// behind the session's earlier records must not eat it.
		ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
		err := p.deliver(ctx, ev)
		cancel()

		p.mu.Lock()
		p.outstanding--
		if err == nil {
			p.mu.Unlock()
			continue
		}
		rest := len(p.queues[session])
		p.outstanding -= rest
		delete(p.queues, session)
		p.mu.Unlock()
		atomic.AddUint64(&p.dropped, uint64(rest))
		tracePoolDropTail(session, rest)
		return
	}
}

// Dropped is the count of records the pool could not accept because it was
// saturated, plus any Close abandoned at its deadline, for doctor's
// disclosure of the one-attempt-per-record accepted risk.
func (p *DeliverPool) Dropped() uint64 { return atomic.LoadUint64(&p.dropped) }

// Close stops the pool accepting new submissions and blocks until every
// accepted record -- in flight or still queued behind its session -- is
// finished, or ctx's deadline passes, whichever comes first. A caller should
// give ctx a deadline no later than the pool's own per-emit timeout: waiting
// any longer cannot help (a delivery past that point has already given up on
// its own context) and only delays process exit.
//
// Without this, a lane daemon's shutdown (rec.Shutdown/srv.Shutdown, then
// os.Exit) stops accepting new HTTP requests but does nothing about a
// delivery Submit already accepted: os.Exit kills that goroutine mid-flight,
// and because only saturation was ever counted as "dropped", the shutdown log
// could read dropped=0 while a record was silently lost. Close closes that
// gap: whatever is still outstanding when ctx is done is abandoned and
// counted through the same Dropped() counter, so `doctor`'s one number covers
// both loss modes a shutdown can hit. A queued record is also taken out of
// its queue, so no attempt starts after the deadline, and handed to
// OnAbandon. Safe to call once; a later call is a no-op returning 0.
func (p *DeliverPool) Close(ctx context.Context) (abandoned int) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0
	}
	p.closed = true
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return 0
	case <-ctx.Done():
	}

	// Emptying a queue leaves its key present: that session's drainer still
	// owns it and releases it when it next finds the queue empty.
	var never []client.DevEvent
	p.mu.Lock()
	for session, queued := range p.queues {
		never = append(never, queued...)
		p.queues[session] = nil
	}
	p.outstanding -= len(never)
	// Best-effort by nature (an in-flight delivery can finish the moment
	// after this read), which is fine for a shutdown-time diagnostic count,
	// not a precise ledger.
	abandoned = p.outstanding + len(never)
	p.mu.Unlock()

	if abandoned > 0 {
		atomic.AddUint64(&p.dropped, uint64(abandoned))
	}
	for _, ev := range never {
		traceQueueAbandon(ev)
	}
	if p.OnAbandon != nil {
		for _, ev := range never {
			p.OnAbandon(ev)
		}
	}
	return abandoned
}
