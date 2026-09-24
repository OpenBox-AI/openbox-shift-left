package hookflow

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// DefaultPoolSize and DefaultPoolTimeout size the transport lane's own
// claude.ai CHAT delivery pool (8 concurrent, 10s per emit, matching the
// gate's evaluateTimeout). A chat conversation has no session-scoped spool
// of its own (chats are not tool sessions; see sessionkey.IsChatKey) and no
// flusher behind it, so this pool's own backpressure is the only one this
// path has.
const (
	DefaultPoolSize    = 8
	DefaultPoolTimeout = 10 * time.Second
)

// DeliverPool bounds concurrent in-process delivery for a lane daemon that
// sends records itself instead of spooling them for a flusher: never the
// request goroutine, one attempt per record, a per-emit timeout, and a
// dropped-record counter `doctor` can read.
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
	sem     chan struct{}
	timeout time.Duration
	deliver func(ctx context.Context, ev client.DevEvent)
	dropped uint64
	wg      sync.WaitGroup
	closed  atomic.Bool
}

// NewDeliverPool builds a pool of size concurrent in-flight deliveries, each
// bounded by timeout, calling deliver for every accepted record. size<=0 and
// timeout<=0 fall back to the defaults above.
func NewDeliverPool(size int, timeout time.Duration, deliver func(ctx context.Context, ev client.DevEvent)) *DeliverPool {
	if size <= 0 {
		size = DefaultPoolSize
	}
	if timeout <= 0 {
		timeout = DefaultPoolTimeout
	}
	return &DeliverPool{
		sem:     make(chan struct{}, size),
		timeout: timeout,
		deliver: deliver,
	}
}

// Submit tries to accept ev for background delivery and returns immediately;
// it never blocks the caller on a network round trip. A full pool is a DROP,
// counted rather than queued: this accepts losing a record under load rather
// than growing an unbounded backlog in a process that has no spool to hold
// it. false means dropped.
func (p *DeliverPool) Submit(ev client.DevEvent) bool {
	if p.closed.Load() {
		// Close has started draining; admitting more work here would be
		// something Close never waits for, and process exit right behind it
		// would kill it mid-goroutine exactly like the shutdown race Close
		// exists to close. Counted the same as a saturation drop: either way
		// the record never reached core.
		atomic.AddUint64(&p.dropped, 1)
		return false
	}
	select {
	case p.sem <- struct{}{}:
	default:
		atomic.AddUint64(&p.dropped, 1)
		return false
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() { <-p.sem }()
		ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
		defer cancel()
		p.deliver(ctx, ev)
	}()
	return true
}

// Dropped is the count of records the pool could not accept because it was
// saturated, plus any Close abandoned at its deadline, for doctor's
// disclosure of the one-attempt-per-record accepted risk.
func (p *DeliverPool) Dropped() uint64 { return atomic.LoadUint64(&p.dropped) }

// Close stops the pool accepting new submissions and blocks until every
// in-flight delivery finishes or ctx's deadline passes, whichever comes
// first. A caller should give ctx a deadline no later than the pool's own
// per-emit timeout: waiting any longer cannot help (a delivery past that
// point has already given up on its own context) and only delays process
// exit.
//
// Without this, a lane daemon's shutdown (rec.Shutdown/srv.Shutdown, then
// os.Exit) stops accepting new HTTP requests but does nothing about a
// delivery Submit already accepted: os.Exit kills that goroutine mid-flight,
// and because only saturation was ever counted as "dropped", the shutdown log
// could read dropped=0 while a record was silently lost. Close closes that
// gap: whatever is still in flight when ctx is done is abandoned and counted
// through the same Dropped() counter, so `doctor`'s one number covers both
// loss modes a shutdown can hit. Safe to call once; a later call is a no-op
// returning 0.
func (p *DeliverPool) Close(ctx context.Context) (abandoned int) {
	if !p.closed.CompareAndSwap(false, true) {
		return 0
	}
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return 0
	case <-ctx.Done():
		// len(p.sem) is the number of semaphore tokens currently held, i.e.
		// deliveries that acquired one via Submit and have not yet released it
		// -- the in-flight count at this instant. Best-effort by nature (a
		// delivery can finish the moment after this read), which is fine for a
		// shutdown-time diagnostic count, not a precise ledger.
		abandoned = len(p.sem)
		if abandoned > 0 {
			atomic.AddUint64(&p.dropped, uint64(abandoned))
		}
		return abandoned
	}
}
