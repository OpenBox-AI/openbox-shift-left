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
//
// An unlockable filesystem -- the lock file itself cannot be opened, not
// merely held -- proceeds unlocked with one log line, the same fail-open
// posture lockSpool takes for the directory-level lock (spool.go). That is
// different from a Block wait simply running out of ITS OWN ctx budget while
// the stripe stays genuinely held: that is reported as an ordinary ctx-cut
// pass (err == ctx.Err()), not a locking fault.
func (s Spool) lockSession(ctx context.Context, sessionID string, mode DrainMode) (release func(), err error) {
	path := s.sessionStripePath(sessionID)
	fl := flock.New(path)
	noop := func() {}

	var ok bool
	if mode == Try {
		ok, err = fl.TryLock()
	} else {
		ok, err = fl.TryLockContext(ctx, sessionStripeRetry)
	}
	switch {
	case ok:
		return func() { _ = fl.Unlock() }, nil
	case err != nil && ctx.Err() != nil:
		return noop, ctx.Err()
	case err != nil:
		s.logf("spool: %s stripe lock unavailable, draining unlocked: %v", filepath.Base(path), err)
		return noop, nil
	default:
		// mode == Try and the stripe is genuinely held by another drainer.
		return noop, ErrSessionBusy
	}
}
