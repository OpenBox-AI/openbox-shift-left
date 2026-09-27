package hookflow

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// Spool decouples the tool-call hot path from the network.
//
// The control plane does not dedupe developer events on their id, so anything
// that reaches it outside this spool has to avoid duplicating itself on its own.
// That is the rule the double-store bug broke.
//
// Every event gets ONE delivery attempt per pass, in append order, plus
// exactly one retry when that attempt failed transiently
// (client.RetryableDelivery). An event still unaccepted after that goes to
// OnFailure and is gone. What a pass could not even START, or could not fit
// its retry into (its budget ran out first), stays queued, in a head file
// ahead of the tail, unscored: the next pass gives it a fresh attempt and
// retry, so a short pass (a gate's drain slack) can send one event more than
// twice before its run halts.
type Spool struct {
	Dir string
	// Log reports a loss the spool cannot avoid. Nil ⇒ silent. Reported here rather
	// than diffed by a caller: the discard log is size-capped and RESTARTS.
	Log func(format string, args ...any)
	// OnFailure is invoked once for every event a drain ATTEMPTED and did not
	// get an accepted verdict for -- any error at all (transport, timeout, a
	// judged refusal, everything). Nil ⇒ defaultOnFailure, a ledger line
	// naming client.FailureClass(err); a later caller additionally writes the
	// run-keyed halt latch here.
	OnFailure func(ev client.DevEvent, err error)
}

func (s Spool) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

// onFailure runs s.OnFailure, or the default ledger line when unset.
func (s Spool) onFailure(ev client.DevEvent, err error) {
	if s.OnFailure != nil {
		s.OnFailure(ev, err)
		return
	}
	s.defaultOnFailure(ev, err)
}

// errOrphanedDrain is the cause OnFailure sees for a line reclaimed from a
// drainer that died mid-pass: its outcome cannot be proven, so it is
// treated as a failure like any other, with its own wording rather than a
// network/timeout class that would misdescribe why nothing arrived.
var errOrphanedDrain = errors.New("hookflow: a drainer died mid-delivery; the outcome of this event cannot be proven")

func (s Spool) defaultOnFailure(ev client.DevEvent, err error) {
	reason := "a drainer died mid-delivery; the outcome of this event cannot be proven"
	if !errors.Is(err, errOrphanedDrain) {
		reason = "not accepted by core: " + client.FailureClass(err)
	}
	s.recordDiscard(s.SessionPath(ev.SessionID), 1, reason)
}

// Append writes one event as a single JSON line to the session's spool file.
//
// O_APPEND guarantees the atomic offset update, so two hook processes never
// interleave within a line. What it does not guarantee, and what the lock is
// for, is that the line survives: a concurrent drain renames this path aside,
// reads it and removes it, so a write landing between the read and the remove
// goes into a file nothing reads again -- a tool call with no evidence.
//
// The lock is a sidecar, never the JSONL file: a drain opens that path too,
// and locking it would deadlock against it.
func (s Spool) Append(ev client.DevEvent) error {
	if err := s.appendLine(s.SessionPath(ev.SessionID), "", ev); err != nil {
		return err
	}
	traceSpoolAppend(ev)
	return nil
}

// appendLine writes ev as one JSON line to path under the spool's directory
// lock. label names the file in the open and write errors ("" for the
// session's tail, " head" for its head file).
func (s Spool) appendLine(path, label string, ev client.DevEvent) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("spool mkdir: %w", err)
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("spool marshal: %w", err)
	}
	line = append(line, '\n')

	unlock := s.lockSpool()
	defer unlock()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("spool open%s: %w", label, err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("spool write%s: %w", label, err)
	}
	return nil
}

// SpoolLockName is the sidecar every append and rotate serializes on. One file
// for the directory, not one per session, which would accumulate forever; the
// hold is one write or one rename either way. The leading dot and absent
// `.jsonl` suffix keep it out of every sweep that reads this directory.
const SpoolLockName = ".spool.lock"

// lockSpool takes the directory's sidecar lock and returns its release. It
// blocks, which is what INV-3 wants: a hook that gave up would drop the event
// rather than pause, and the hold is never more than one write or one rename.
// A filesystem that cannot lock at all yields a no-op release rather than an
// error -- the same fail-open posture as the installer's "cannot lock here".
func (s Spool) lockSpool() func() {
	fl := flock.New(filepath.Join(s.Dir, SpoolLockName))
	if err := fl.Lock(); err != nil {
		return func() {}
	}
	return func() { _ = fl.Unlock() }
}

// FlushFunc delivers one spooled event.
type FlushFunc func(context.Context, client.DevEvent) error

// DeliveryAttemptTimeout bounds ONE delivery attempt: 30s, core's own
// workflow cap. It is the attemptTimeout every non-inline drainer (the
// detached flusher, the sweeper, uninstall's flush) passes to DrainSession;
// a gate escalation and an inline SessionStart/SessionEnd attempt pass their
// own, smaller budget instead.
const DeliveryAttemptTimeout = 30 * time.Second

// HeadSuffix names the file a cut pass writes its unattempted remainder to,
// always ahead of the tail: at most one per stem.
const HeadSuffix = ".head.jsonl"

func (s Spool) headPath(sessionID string) string {
	return filepath.Join(s.Dir, sanitizeSessionID(sessionID)+HeadSuffix)
}

// ReclaimOrphanAfter bounds "no drain could still hold this"; drains take
// seconds, so a `.flushing.` rotation older than this proves its drainer died
// mid-pass rather than merely being slow.
const ReclaimOrphanAfter = 5 * time.Minute

// DrainSession drains one session's queue -- its head file, then its tail --
// through fn, giving every line one delivery attempt in order, plus one retry
// for a transient failure (attemptLines). Held
// across the whole call is sessionID's stripe lock (mode selects Block/Try;
// see DrainMode), so two drainers never deliver the same session
// concurrently and delivery order always equals append order.
//
// Before draining, any aged orphan (a prior drainer that died mid-pass, or a
// legacy pre-single-attempt carry-over file) belonging to this session is
// reclaimed and discarded -- never redelivered.
//
// attemptTimeout bounds each individual delivery; a pass never starts an
// attempt it cannot finish: once ctx's remaining budget drops below
// attemptTimeout, everything left is written to the session's head file
// instead of being attempted (not a failure). The returned error, when
// non-nil, reports that cutoff (or ctx's own error); it never means an event
// was lost.
func (s Spool) DrainSession(ctx context.Context, sessionID string, fn FlushFunc, opts DrainOptions) (int, error) {
	release, lockErr := s.lockSession(ctx, sessionID, opts.Mode, opts.LockWait)
	defer release()
	if lockErr != nil {
		return 0, lockErr
	}

	// A pass that cannot even cover one attempt must not touch anything: past
	// this point the pass rotates the tail (and any head file) aside, which
	// resets their mtime to "now" whether or not anything in them was
	// attempted. A caller (FlushAll, the periodic sweep) walking a directory
	// under a shrinking ctx would otherwise churn every stem's mtime every
	// pass without ever attempting most of them, which makes retirement
	// think every file just got its shot and a genuinely starving one never
	// comes due.
	if remaining, ok := ctxRemaining(ctx); ok && remaining < opts.AttemptTimeout {
		return 0, errPassCutShort
	}

	stem := sanitizeSessionID(sessionID)
	s.reclaimStemLegacy(stem)
	// Reclaimed, not delivered: never added to the count this returns (see
	// below).
	s.reclaimStemOrphans(stem, s.onFailure)

	lines, rotated, err := s.collectSession(sessionID)
	removeRotated := func() {
		// Only after the remainder (if any) is durably queued in the head
		// file: a process killed between collectSession and here (Codex
		// SIGKILLs a SessionEnd hook at 3s) leaves these rotated files on
		// disk exactly as any other `.flushing.` rotation, so the next
		// drainer's reclaimStemOrphans finds and discards them past
		// ReclaimOrphanAfter instead of the lines vanishing with no ledger
		// line and nothing for a halt to hang off of.
		for _, p := range rotated {
			_ = os.Remove(p)
		}
	}
	if err != nil {
		return 0, err
	}
	if len(lines) == 0 {
		removeRotated()
		return 0, nil
	}

	delivered, corrupt, remainder, cutoff := s.attemptLines(ctx, lines, fn, opts)
	if corrupt > 0 {
		// Its own reason, so an investigation goes to the writer rather than to the
		// wire: a line that will not parse was damaged on disk, not refused.
		s.recordDiscard(s.SessionPath(sessionID), corrupt, "the spooled line was not valid JSON and could not be read back")
	}
	s.writeHead(sessionID, remainder)
	removeRotated()

	// orphaned lines were reclaimed and discarded, never delivered, so they
	// are never added to the returned count: every caller (the drain-until-
	// empty loop, a flush's own "how many did I send" log line) reads this as
	// "accepted by core", not "lines this pass touched".
	if !cutoff {
		return delivered, nil
	}
	if ctx.Err() != nil {
		return delivered, ctx.Err()
	}
	return delivered, errPassCutShort
}

// ctxRemaining reports how long ctx has left, and whether it even has a
// deadline: a context with none (context.Background(), or one only ever
// cancelled explicitly) never counts as short on time.
func ctxRemaining(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	return time.Until(deadline), true
}

// DrainOptions configures one DrainSession call.
type DrainOptions struct {
	// Mode selects Block or Try (sessionlock.go) for this call's own
	// session stripe.
	Mode DrainMode
	// AttemptTimeout bounds each individual delivery attempt this call
	// makes; a pass never starts one it cannot finish (see DrainSession).
	AttemptTimeout time.Duration
	// RequeueUnanswered changes what an attempt that both errored AND never
	// heard back within its own attemptTimeout window means: normally (the
	// detached flusher, the sweep, uninstall's flush, all with a generous
	// 30s attemptTimeout) that is treated as any other failure -- an
	// explicit answer just never arrived, and 30s is long enough that
	// silence itself is meaningful. An inline SessionStart/SessionEnd
	// attempt uses a much smaller window purely to avoid holding up the
	// hook, so silence there proves nothing about whether core would have
	// accepted the event; with this set, such a line (and everything after
	// it, to keep order intact) is left queued for the detached flusher's
	// own, unbounded try instead of being scored as a failure.
	RequeueUnanswered bool
	// LockWait bounds ONLY a Block mode caller's own wait for the session's
	// stripe (sessionlock.go's own lockSession): 0 (every caller before this
	// field existed) leaves the wait bounded by ctx alone; a positive value
	// additionally gives up once that much time has passed even if ctx
	// itself still has budget left. Ignored in Try mode, which never waits
	// at all. A caller whose own stripe-wait bound must be strictly
	// narrower than its overall ctx (a gate's own drain step, sharing its
	// session's stripe with a live flusher or lane daemon that may be
	// mid-delivery on a slow event) sets this; the drain PASS itself, once
	// the stripe is acquired, still gets everything ctx leaves it.
	LockWait time.Duration
}

// errPassCutShort reports that a pass's own budget ran out before another
// attempt could safely start: the remainder is queued in the head file,
// not lost, and this is never treated as an event failure.
var errPassCutShort = errors.New("hookflow: the pass's budget ran out before another attempt could safely start")

// collectSession rotates the session's head file (if any) and tail aside
// under the directory lock -- the same one Append takes -- so a concurrent
// Append lands in a fresh, empty tail and never interleaves with what this
// pass is about to read. It returns their lines combined, head before tail,
// because the head is always the earlier, previously-unattempted remainder,
// plus the rotated paths themselves -- which the caller must NOT remove
// until whatever it decides to do with the lines (deliver them, queue a
// remainder) is durable. Removing them here, before that, is exactly the
// crash window that used to lose lines with no ledger entry and no orphan
// left for reclaimStemOrphans to find: a process killed between this read
// and the caller's own cleanup would otherwise take the only copy of
// whatever it had not yet attempted down with it. Both rotated copies are
// rotated together so a crash before either is removed leaves two ordinary
// `.flushing.` orphans, reclaimed (never redelivered) the same way as any
// other -- never a stale, silently-stale head file.
func (s Spool) collectSession(sessionID string) (lines [][]byte, rotatedPaths []string, err error) {
	head := s.headPath(sessionID)
	tail := s.SessionPath(sessionID)
	rotatedHead := head + ".flushing." + rand.Text()
	rotatedTail := tail + ".flushing." + rand.Text()

	rotateErr := func() error {
		unlock := s.lockSpool()
		defer unlock()

		now := time.Now()
		if renameErr := os.Rename(head, rotatedHead); renameErr != nil {
			if !os.IsNotExist(renameErr) {
				return fmt.Errorf("spool rotate head: %w", renameErr)
			}
			rotatedHead = ""
		} else {
			_ = os.Chtimes(rotatedHead, now, now)
		}
		if renameErr := os.Rename(tail, rotatedTail); renameErr != nil {
			if !os.IsNotExist(renameErr) {
				return fmt.Errorf("spool rotate tail: %w", renameErr)
			}
			rotatedTail = ""
		} else {
			_ = os.Chtimes(rotatedTail, now, now)
		}
		return nil
	}()
	if rotateErr != nil {
		return nil, nil, rotateErr
	}

	var errs []error
	for _, path := range []string{rotatedHead, rotatedTail} {
		if path == "" {
			continue
		}
		data, rerr := os.ReadFile(path)
		switch {
		case rerr == nil:
			lines = append(lines, NonEmptyLines(data)...)
			// Read successfully, so the caller may remove it once it is done
			// with the lines. A read failure leaves the path OUT of this
			// slice on purpose: the caller must never delete a file it could
			// not actually read.
			rotatedPaths = append(rotatedPaths, path)
		case !os.IsNotExist(rerr):
			errs = append(errs, fmt.Errorf("spool read: %w", rerr))
		}
	}
	return lines, rotatedPaths, errors.Join(errs...)
}

// writeHead atomically replaces the session's head file with remainder, or
// removes it when remainder is empty: at most one head file per stem, always
// the sole record of what a pass could not even start.
//
// It takes the directory lock and MERGES rather than blindly overwrites:
// collectSession rotated the previous head file (if any) away before this
// pass began, so a fresh write straight to headPath in the meantime -- a
// gate's own escalation requeuing an unanswered attempt, SpoolObserveHead --
// is strictly NEWER than remainder (drawn from what was rotated away at the
// START of this pass) and must survive AFTER it, never silently replaced.
func (s Spool) writeHead(sessionID string, remainder [][]byte) {
	path := s.headPath(sessionID)

	unlock := s.lockSpool()
	defer unlock()

	existing, _ := os.ReadFile(path)
	lines := append(append([][]byte{}, remainder...), NonEmptyLines(existing)...)
	if len(lines) == 0 {
		_ = os.Remove(path)
		return
	}
	var buf bytes.Buffer
	for _, l := range lines {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	tmp := path + ".tmp." + rand.Text()
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		_ = os.Remove(tmp)
		s.logf("spool: %s: the unattempted remainder could not be written to its head file: %v", filepath.Base(path), err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		s.logf("spool: %s: the unattempted remainder could not be renamed into its head file: %v", filepath.Base(path), err)
	}
}

// SpoolObserveHead appends ev's observe copy directly to the session's head
// file, ahead of its tail: for a gate escalation whose own budget ran out
// before core answered (the same unanswered-vs-failed discriminator
// DrainOptions.RequeueUnanswered uses), the outcome is unknown, so it is
// requeued for a drainer with a real, unbounded attempt rather than scored as
// a failure -- never latched, never counted as a second attempt against this
// event (core dedupes the eventual re-send on the idempotency key).
//
// Shares Append's own directory lock, so it is race-safe against a
// concurrent rotate (collectSession) and a concurrent writeHead's own merge:
// whichever holds the lock first completes atomically before the other
// proceeds.
func (s Spool) SpoolObserveHead(ev client.DevEvent) error {
	return s.appendLine(s.headPath(ev.SessionID), " head", ev)
}

// attemptLines gives each line one delivery attempt, in order, plus exactly
// one retry when that attempt failed transiently (client.RetryableDelivery:
// timeout, network, 5xx); accepted or not after that, it advances to the next
// line via onFailure. A retry the pass cannot fit -- less than attemptTimeout
// left -- is not attempted: the line and everything after it stay queued for
// the next pass, never scored as a failure it was not allowed to retry. Two
// other things
// stop the loop early, both reported back as a cutoff rather than a failure
// of any line left in remainder: ctx running out (its own error, or its
// remaining budget dropping below attemptTimeout before another attempt
// could safely start), and -- only when opts.RequeueUnanswered is set -- an
// attempt whose own per-line ctx expired before fn returned an error. That
// second case is silence, not an answer: core may have accepted or refused
// the event and the process never found out, so the line (and everything
// after it, so a later line can never overtake an earlier one whose outcome
// is unknown) is left for a drainer with a real, unbounded attempt to try
// instead of being scored as a failure.
func (s Spool) attemptLines(ctx context.Context, lines [][]byte, fn FlushFunc, opts DrainOptions) (delivered, corrupt int, remainder [][]byte, cutoff bool) {
	for i, line := range lines {
		if ctx.Err() != nil {
			return delivered, corrupt, lines[i:], true
		}
		if remaining, ok := ctxRemaining(ctx); ok && remaining < opts.AttemptTimeout {
			return delivered, corrupt, lines[i:], true
		}

		var ev client.DevEvent
		if json.Unmarshal(line, &ev) != nil {
			corrupt++
			continue
		}
		derr, unanswered := attemptOne(ctx, fn, ev, opts.AttemptTimeout)
		if derr != nil && !(opts.RequeueUnanswered && unanswered) && client.RetryableDelivery(derr) {
			if remaining, ok := ctxRemaining(ctx); ctx.Err() != nil || (ok && remaining < opts.AttemptTimeout) {
				return delivered, corrupt, lines[i:], true
			}
			s.logf("spool: %s for %s not accepted (%s); retrying once", ev.EventType, ev.SessionID, client.FailureClass(derr))
			derr, unanswered = attemptOne(WithDeliveryAttempt(ctx, 2), fn, ev, opts.AttemptTimeout)
		}
		if derr != nil {
			if opts.RequeueUnanswered && unanswered {
				return delivered, corrupt, lines[i:], true
			}
			s.onFailure(ev, derr)
			continue
		}
		delivered++
	}
	return delivered, corrupt, nil, false
}

// attemptOne makes one delivery attempt bounded by attemptTimeout, and
// reports whether that attempt's own deadline expired before fn returned
// (read before cancel can change it), the unanswered-vs-answered signal
// DrainOptions.RequeueUnanswered relies on.
func attemptOne(ctx context.Context, fn FlushFunc, ev client.DevEvent, attemptTimeout time.Duration) (err error, unanswered bool) {
	attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()
	err = fn(attemptCtx, ev)
	return err, attemptCtx.Err() != nil
}

// reclaimStemLegacy discards every legacy (pre-single-attempt) carry-over
// file belonging to stem on sight: this release has no carry-over concept, so
// a `<stem>.recN-*.jsonl` left by an old binary is never drained, only
// counted and removed.
func (s Spool) reclaimStemLegacy(stem string) {
	prefix := stem + ".rec"
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		base := name
		if i := strings.Index(base, ".flushing."); i >= 0 {
			base = base[:i]
		}
		if !IsRecoveryFile(base) {
			continue
		}
		path := filepath.Join(s.Dir, name)
		n := countLines(path)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			continue
		}
		if n > 0 {
			s.recordDiscard(path, n, "a carry-over file from a prior release; single-attempt "+
				"delivery has no carry-over, so it is discarded rather than drained")
		}
	}
}

// reclaimStemOrphans discards every `.flushing.` rotation belonging to stem
// that is old enough to prove its drainer died mid-pass (ReclaimOrphanAfter):
// the outcome of its lines cannot be known, so they are never redelivered,
// only counted and, through onFailure, individually failed so a later phase's
// latch can halt the run they belonged to. Returns how many parseable
// lines it reclaimed.
func (s Spool) reclaimStemOrphans(stem string, onFailure func(client.DevEvent, error)) int {
	prefixes := [2]string{stem + ".jsonl.flushing.", stem + HeadSuffix + ".flushing."}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return 0
	}
	total := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		matched := false
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		info, statErr := e.Info()
		if statErr != nil || time.Since(info.ModTime()) < ReclaimOrphanAfter {
			continue
		}
		path := filepath.Join(s.Dir, name)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue // already gone: nothing to reclaim
			}
			// Unreadable for a reason that will not resolve itself
			// (permissions, a damaged directory entry): its lines are
			// neither delivered nor decodable, so no event and no run is
			// known to fail (never OnFailure, never a latch) -- but
			// silently skipping it, forever, on every future pass is its
			// own loss. Ledgered once, by file, then removed so the next
			// pass never sees it again.
			s.recordDiscard(path, 1, fmt.Sprintf("an orphaned spool file could not be read back and is discarded: %v", readErr))
			_ = os.Remove(path)
			continue
		}
		lines := NonEmptyLines(data)
		corrupt := 0
		for _, line := range lines {
			var ev client.DevEvent
			if json.Unmarshal(line, &ev) != nil {
				corrupt++
				continue
			}
			if onFailure != nil {
				onFailure(ev, errOrphanedDrain)
			}
		}
		_ = os.Remove(path)
		if corrupt > 0 {
			s.recordDiscard(path, corrupt, "the spooled line was not valid JSON and could not be read back")
		}
		total += len(lines) - corrupt
	}
	return total
}

// IsRecoveryFile reports whether name is a LEGACY (pre-single-attempt)
// carry-over file this release never writes again: `<session>.rec<N>-<id>.jsonl`,
// or the pre-attempt-counter `<session>.rec-<id>.jsonl`. Kept only so a file an
// old binary left behind is recognized and discarded, never drained.
func IsRecoveryFile(name string) bool {
	if !strings.HasSuffix(name, ".jsonl") || strings.Contains(name, ".flushing.") {
		return false
	}
	i := strings.Index(name, ".rec")
	if i < 0 {
		return false
	}
	rest := name[i+len(".rec"):]
	j := strings.Index(rest, "-")
	if j < 0 {
		return false
	}
	for _, r := range rest[:j] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// recoveryStem returns the canonical `<dir>/<session>` stem for a legacy
// recovery filename, dropping its `.rec<N>-<id>` segment.
func recoveryStem(basePath string) string {
	dir, name := filepath.Split(basePath)
	name = strings.TrimSuffix(name, ".jsonl")
	if i := strings.Index(name, ".rec"); i >= 0 {
		name = name[:i]
	}
	return filepath.Join(dir, name)
}

// sessionStemFromName recovers a stem from any file this package writes for a
// session -- tail, head, an in-flight rotation of either, or a legacy
// pre-single-attempt carry-over -- so FlushAll can group every variant under
// the one stem DrainSession expects. false for a directory-level file (the
// spool lock, the discard log, a session's flush-debounce lock, a session
// stripe lock) nothing owns a session.
func sessionStemFromName(name string) (string, bool) {
	switch name {
	case SpoolLockName, DiscardLogName:
		return "", false
	}
	if strings.HasSuffix(name, ".flushlock") || strings.HasPrefix(name, ".session.lock.") {
		return "", false
	}
	if i := strings.Index(name, ".flushing."); i >= 0 {
		name = name[:i]
	}
	if IsRecoveryFile(name) {
		return recoveryStem(name), true
	}
	if strings.HasSuffix(name, HeadSuffix) {
		return strings.TrimSuffix(name, HeadSuffix), true
	}
	if strings.HasSuffix(name, ".jsonl") {
		return strings.TrimSuffix(name, ".jsonl"), true
	}
	return "", false
}

// FlushAll drains every session spool in the directory -- the `flush`
// subcommand's no-session sweep, and the periodic Sweeper's catch-up -- each
// through DrainSession in Try mode: FlushAll does not own any of these
// sessions, so a stripe another drainer already holds is skipped this pass,
// not waited on.
func (s Spool) FlushAll(ctx context.Context, fn FlushFunc) (int, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // nothing spooled yet
		}
		return 0, fmt.Errorf("spool readdir: %w", err)
	}
	// entries is already alphabetical (os.ReadDir sorts), and callers rely on
	// that: a pass cut short by ctx must reach an earlier name before a later
	// one, so retirement can tell "never reached" apart from "reached and
	// still live". seen dedupes without reordering -- one stem can surface
	// from several names (its tail, its head file, an in-flight rotation).
	stems := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		stem, ok := sessionStemFromName(e.Name())
		if !ok || seen[stem] {
			continue
		}
		seen[stem] = true
		stems = append(stems, stem)
	}

	total := 0
	var errs []error
	for _, stem := range stems {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		n, derr := s.DrainSession(ctx, stem, fn, DrainOptions{Mode: Try, AttemptTimeout: DeliveryAttemptTimeout})
		total += n
		switch {
		case derr == nil, errors.Is(derr, ErrSessionBusy):
		case errors.Is(derr, errPassCutShort):
			// Every stem still to walk shares this same ctx and would see
			// the identical (or smaller) remaining budget, so stop here
			// rather than touch, and instantly reject, each one in turn.
			errs = append(errs, derr)
			return total, errors.Join(errs...)
		default:
			errs = append(errs, derr)
		}
	}
	// One session's spool failing must not hide the next one's; errors.Join
	// drops the nils, so a clean pass still returns nil.
	return total, errors.Join(errs...)
}

// UndeliveredCount reports how many events are currently queued in a head
// file -- attempted-too-late-to-start, waiting for the next pass -- across
// every session in the directory. Best-effort; an unreadable directory
// reports 0, because this feeds a telemetry field and must never fail a
// session.
func (s Spool) UndeliveredCount() int {
	return s.sumLines(func(n string) bool { return strings.HasSuffix(n, HeadSuffix) })
}

// UndeliveredCountFor narrows UndeliveredCount to one session's own pending
// head-plus-tail count (SessionEnd's EvidenceState.Undelivered): its queue,
// not another session's backlog sitting in the same directory, and not a
// count of what already failed and was ledgered -- a single-attempt failure
// is gone, not pending.
func (s Spool) UndeliveredCountFor(sessionID string) int {
	return s.PendingCount(sessionID)
}

func (s Spool) sumLines(match func(name string) bool) int {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return 0
	}
	total := 0
	for _, e := range entries {
		if e.IsDir() || !match(e.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			continue
		}
		total += len(NonEmptyLines(data))
	}
	return total
}

// NonEmptyLines the returned slice is the caller's to index -- attemptLines
// carries `lines[i:]` into the head file on cancellation -- so the signature
// stays. SplitSeq only removes the intermediate index of every line including
// the empty ones, which on a spool of small lines is most of the allocation.
func NonEmptyLines(data []byte) [][]byte {
	var out [][]byte
	for l := range bytes.SplitSeq(data, []byte{'\n'}) {
		if len(l) > 0 {
			out = append(out, l)
		}
	}
	return out
}

// SessionPath is the spool file for a session id, sanitized for the
// filesystem.
func (s Spool) SessionPath(sessionID string) string {
	return filepath.Join(s.Dir, sanitizeSessionID(sessionID)+".jsonl")
}

// FlushLockPath is the per-session debounce lockfile the RealtimeTrigger and
// the spawned flusher coordinate through.
func (s Spool) FlushLockPath(sessionID string) string {
	return filepath.Join(s.Dir, sanitizeSessionID(sessionID)+".flushlock")
}

// TouchFlushLock refreshes (creating if needed) the session's flush lock, so
// the debounce window covers a running drain, not just its spawn.
func (s Spool) TouchFlushLock(sessionID string) {
	lock := s.FlushLockPath(sessionID)
	now := time.Now()
	if os.Chtimes(lock, now, now) == nil {
		return
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return
	}
	if f, err := os.OpenFile(lock, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
	}
}

// ReleaseFlushLock removes the session's flush lock when a drain finishes, so
// the next spooled event can trigger a fresh flusher immediately instead of
// waiting out the debounce window.
func (s Spool) ReleaseFlushLock(sessionID string) {
	_ = os.Remove(s.FlushLockPath(sessionID))
}

func sanitizeSessionID(id string) string {
	if id == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
