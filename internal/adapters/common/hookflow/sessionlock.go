package hookflow

import (
	"context"
	"errors"
	"hash/fnv"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gofrs/flock"
)

// SessionStripes bounds how many sessions can drain concurrently while still
// using ONE lock file per stripe rather than one per session (which would
// accumulate forever). Two unrelated sessions hashing to the same stripe
// couple their drains -- accepted at this width; raise it (256 is the next
// step) if a fleet measurement ever shows that coupling above 1% of drains.
const SessionStripes = 64

// sessionStripeRetry is how often a Block wait re-polls its stripe.
const sessionStripeRetry = 10 * time.Millisecond

// DrainMode selects how DrainSession behaves when its session's stripe is
// already held by another drainer.
type DrainMode int

const (
	// Block waits, bounded by ctx, for the stripe: the caller owns this
	// session (its own SessionEnd flush, its own inline SessionStart/
	// SessionEnd attempt, a gate's own escalation) and must not skip its turn.
	Block DrainMode = iota
	// Try gives up at once: the caller is sweeping every OTHER session's
	// backlog (FlushAll, the periodic sweeper) and one busy stripe must not
	// stall the rest.
	Try
)

// ErrSessionBusy reports that a Try drain found its session's stripe already
// held by another drainer. Nothing was read, nothing was sent.
var ErrSessionBusy = errors.New("hookflow: session drain already in progress")

// sessionStripePath maps a session id to its stripe's lock file. Collisions
// past SessionStripes are by design (see the const doc); fnv32a is fast and
// stable, not cryptographic, which is all a lock selector needs.
func (s Spool) sessionStripePath(sessionID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(sanitizeSessionID(sessionID)))
	return filepath.Join(s.Dir, ".session.lock."+strconv.Itoa(int(h.Sum32()%SessionStripes)))
}

// lockSession takes sessionID's stripe per mode and returns its release,
// which is always safe to call (a no-op when nothing was actually locked).
// lockWait bounds ONLY the wait for a Block mode caller: 0 leaves the wait
// bounded by ctx alone (every caller before DrainOptions.LockWait existed,
// and every caller today that leaves it unset); a positive value additionally
// gives up once THAT much time has passed, even if ctx itself still has
// budget left -- the gate's own "never wait past this long for a busy
// stripe" bound, distinct from (and always <=) the caller's overall ctx.
//
// An unlockable filesystem -- the lock file itself cannot be opened, not
// merely held -- proceeds unlocked with one log line, the same fail-open
// posture lockSpool takes for the directory-level lock (spool.go). That is
// different from a Block wait simply running out of ITS OWN ctx budget (or
// lockWait) while the stripe stays genuinely held: that is reported as an
// ordinary cut (ctx.Err(), or ErrSessionBusy when it was lockWait rather
// than ctx that ran out), never a locking fault.
func (s Spool) lockSession(ctx context.Context, sessionID string, mode DrainMode, lockWait time.Duration) (release func(), err error) {
	start := time.Now()
	release, err = s.lockSessionUntraced(ctx, sessionID, mode, lockWait)
	traceStripeWait(sessionID, mode, start, stripeWaitOutcome(release, err), err)
	return release, err
}

// stripeWaitOutcome classifies a lockSession result for the trace, without
// relying on error identity alone: a nil release paired with a nil err never
// happens in this function's own contract, but reads as "unlocked" (the
// fail-open filesystem case) rather than crash the trace path over it.
func stripeWaitOutcome(release func(), err error) string {
	switch {
	case err == nil:
		return "acquired"
	case errors.Is(err, ErrSessionBusy):
		return "busy"
	default:
		return "ctx_done"
	}
}

func (s Spool) lockSessionUntraced(ctx context.Context, sessionID string, mode DrainMode, lockWait time.Duration) (release func(), err error) {
	path := s.sessionStripePath(sessionID)
	fl := flock.New(path)
	noop := func() {}

	if mode == Try {
		ok, tryErr := fl.TryLock()
		switch {
		case ok:
			return func() { _ = fl.Unlock() }, nil
		case tryErr != nil:
			s.logf("spool: %s stripe lock unavailable, draining unlocked: %v", filepath.Base(path), tryErr)
			return noop, nil
		default:
			// The stripe is genuinely held by another drainer.
			return noop, ErrSessionBusy
		}
	}

	waitCtx := ctx
	if lockWait > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, lockWait)
		defer cancel()
	}
	ok, lockErr := fl.TryLockContext(waitCtx, sessionStripeRetry)
	switch {
	case ok:
		return func() { _ = fl.Unlock() }, nil
	case ctx.Err() != nil:
		// The CALLER's own ctx ran out (with or without a lockWait set):
		// unchanged from before lockWait existed.
		return noop, ctx.Err()
	case waitCtx.Err() != nil:
		// Only our OWN derived wait (lockWait) elapsed; the caller's ctx
		// still has budget left. The stripe is busy, not a filesystem
		// fault -- report it exactly like Try mode's own busy case, since
		// that is what it is: this caller simply chose not to wait forever.
		return noop, ErrSessionBusy
	case lockErr != nil:
		s.logf("spool: %s stripe lock unavailable, draining unlocked: %v", filepath.Base(path), lockErr)
		return noop, nil
	default:
		return noop, ErrSessionBusy
	}
}
