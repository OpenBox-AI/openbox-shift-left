package hookflow

import (
	"context"
	"log"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// FlushBudget bounds the detached `flush` subcommand: the realtime trigger's
// spawned child, the periodic sweeper, and `openbox uninstall`'s own flush
// all inherit it. 60s so a pass can start DeliveryAttemptTimeout-bounded
// (30s) attempts with slack.
const FlushBudget = 60 * time.Second

// HookClient resolves the hook's Credentials and builds its transport. Either
// failure is logged as a skipped flush and reported as ok=false: the events
// stay spooled for a later pass, so it is never an error the hook surfaces.
func HookClient(logger *log.Logger) (creds Credentials, cl *client.Client, ok bool) {
	creds, err := ResolveCredentials()
	if err != nil {
		logger.Printf("flush skipped (events remain spooled): %v", err)
		return creds, nil, false
	}
	cl, err = creds.NewClient(logger)
	if err != nil {
		logger.Printf("flush skipped (client init): %v", err)
		return creds, nil, false
	}
	return creds, cl, true
}

// ForceFlusher spawns the detached flusher regardless of the realtime_flush
// toggle: once an inline attempt could not finish, waiting out the ordinary
// debounce toggle is not the fallback's job. Reuses RealtimeTrigger.Maybe's
// own spawn/debounce plumbing rather than a second copy of it.
func ForceFlusher(logger *log.Logger, spoolDir, provider, sessionID string) {
	RealtimeTrigger{
		Spool:    Spool{Dir: spoolDir},
		Provider: provider,
		Enabled:  func() bool { return true },
	}.Maybe(logger, sessionID)
}

// InlineAttempt drives one single-attempt, own-session drain within window
// (measured from hookStart, the hook's own process start, so credential
// resolution and everything the hook already did count against the SAME
// budget the hook's own ceiling is), so a session's newest event does not wait
// on the detached flusher's debounce window to reach core. It reuses the
// caller's own Engine (its spool, its advisory sink) rather than building a
// second one, and owns the stripe lock for its own session (Block): the append
// the hook just made is drained under the SAME lock, so nothing can overtake
// it. An attempt whose own ctx expires before core answers is requeued, never
// scored as a failure (RequeueUnanswered): the short inline window proves
// nothing about whether core would have accepted the event. Whatever the
// window could not even start, or could not get an answer for, falls back to
// fallback (the detached flusher) -- not a failure, and a re-send is deduped by
// core's idempotency key.
func (e *Engine) InlineAttempt(logger *log.Logger, sessionID string, hookStart time.Time, window, attemptTimeout time.Duration, fallback func()) {
	_, cl, ok := HookClient(logger)
	if !ok {
		fallback()
		return
	}

	e.Log = logger.Printf
	e.Advisory.Log = logger

	ctx, cancel := context.WithDeadline(context.Background(), hookStart.Add(window))
	defer cancel()
	opts := DrainOptions{Mode: Block, AttemptTimeout: attemptTimeout, RequeueUnanswered: true}
	if _, err := e.DrainSession(ctx, sessionID, cl, opts); err != nil {
		logger.Printf("inline delivery ended early: %v", err)
	}
	if e.Spool.PendingCount(sessionID) > 0 {
		fallback()
	}
}

// RunFlush is the detached `flush` subcommand's body: one FlushOrSweep pass
// over spoolDir, bounded by FlushBudget.
func RunFlush(logger *log.Logger, spoolDir, sessionID string) {
	_, cl, ok := HookClient(logger)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), FlushBudget)
	defer cancel()

	e := NewEngine(spoolDir)
	// The engine's own voice, on the same stderr the flusher's log now captures:
	e.Log = logger.Printf
	// Diagnostics only; stderr, never stdout, so an exit-0 hook still injects
	// nothing (INV-3).
	e.Advisory.Log = logger
	// One engine call: the lock, the drain loop and the retire ordering are its.
	n, err := e.FlushOrSweep(ctx, sessionID, cl)
	if err != nil {
		logger.Printf("flush ended early after %d event(s): %v", n, err)
	}
}

// GateSpoolers returns the gate's two ways of spooling this call's own observe
// copy (EnforceGate.SpoolObserve / SpoolObserveHead): append it, or put it at
// the session's head. devEv must already be duration-threaded -- whichever of
// the two the gate invokes needs the SAME threaded event, and threading it
// twice would double-put its duration-pairing entry. Each nudges the flusher
// after spooling.
func (e *Engine) GateSpoolers(logger *log.Logger, hook string, devEv client.DevEvent, nudge func()) (spoolObserve, spoolObserveHead func()) {
	spoolObserve = func() {
		if err := e.Spool.Append(devEv); err != nil {
			logger.Printf("spool %s event: %v", hook, err)
		}
		nudge()
	}
	spoolObserveHead = func() {
		if err := e.Spool.SpoolObserveHead(devEv); err != nil {
			logger.Printf("spool %s event to the session head: %v", hook, err)
		}
		nudge()
	}
	return spoolObserve, spoolObserveHead
}
