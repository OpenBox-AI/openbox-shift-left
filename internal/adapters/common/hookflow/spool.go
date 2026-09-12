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
	"strconv"
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
type Spool struct {
	Dir string
	// Log reports a loss the spool cannot avoid. Nil ⇒ silent. Reported here rather
	// than diffed by a caller: the discard log is size-capped and RESTARTS.
	Log func(format string, args ...any)
}

func (s Spool) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

// Append writes one event as a single JSON line to the session's spool file.
//
// O_APPEND guarantees the atomic offset update, so two hook processes never
// interleave within a line. What it does not guarantee, and what the lock is
// for, is that the line survives: a concurrent drain renames this path aside,
// reads it and removes it, so a write landing between the read and the remove
// goes into a file nothing reads again -- a tool call with no evidence.
//
// The lock is a sidecar, never the JSONL file: drainFile and the recovery sweep
// open that path too, and locking it would deadlock against them.
func (s Spool) Append(ev client.DevEvent) error {
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

	f, err := os.OpenFile(s.SessionPath(ev.SessionID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("spool open: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("spool write: %w", err)
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

// FlushSession drains one session's spool through fn and returns the number of
// events delivered.
func (s Spool) FlushSession(ctx context.Context, sessionID string, fn FlushFunc) (int, error) {
	return s.drainFile(ctx, s.SessionPath(sessionID), fn)
}

// recoveryFiles an unreadable directory yields none: a sweep is best-effort
// catch-up (observe, INV-3), never a reason to fail a caller.
func (s Spool) recoveryFiles() []string {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && IsRecoveryFile(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// sweepRecovery drains the named carry-over files, those belonging to
// ownSession (if any) first so the ending session's own telemetry gets the
// budget before other sessions' backlog does.
func (s Spool) sweepRecovery(ctx context.Context, names []string, ownSession string, fn FlushFunc) (int, error) {
	if ownSession != "" {
		own := sanitizeSessionID(ownSession) + ".rec"
		mine, theirs := make([]string, 0, len(names)), make([]string, 0, len(names))
		for _, n := range names {
			if strings.HasPrefix(n, own) {
				mine = append(mine, n)
			} else {
				theirs = append(theirs, n)
			}
		}
		names = append(mine, theirs...)
	}
	total := 0
	var errs []error
	for _, name := range names {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		n, err := s.drainFile(ctx, filepath.Join(s.Dir, name), fn)
		total += n
		errs = append(errs, err)
	}
	// Every file's failure, not the first: a sweep that hit one unreadable
	// carry-over and then three more reported one, and the caller had no way to
	// see the rest. errors.Join drops the nils.
	return total, errors.Join(errs...)
}

// IsRecoveryFile reports whether name is a carry-over file;
// `<session>.rec<N>-<id>.jsonl`, or the legacy `<session>.rec-<id>.jsonl`
// written before the attempt counter existed.
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

// FlushAll drains every session spool in the directory; the `flush` subcommand
// / CLI-driven catch-up path; including recovery files (`*.rec-*.jsonl`) left
// by a budget-bounded flush and orphaned `*.flushing.*` files left by a drain
// whose process was killed mid-flight.
func (s Spool) FlushAll(ctx context.Context, fn FlushFunc) (int, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // nothing spooled yet
		}
		return 0, fmt.Errorf("spool readdir: %w", err)
	}
	total := 0
	var errs []error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		name := e.Name()
		var n int
		switch {
		case strings.Contains(name, ".flushing."):
			// A live drain holds its rotated file all through delivery, and nothing dedupes.
			info, statErr := e.Info()
			if statErr != nil || time.Since(info.ModTime()) < ReclaimOrphanAfter {
				continue
			}
			claimed := filepath.Join(s.Dir, name) + ".reclaim." + rand.Text()
			if os.Rename(filepath.Join(s.Dir, name), claimed) != nil {
				continue // lost the race to another drain, or already gone
			}
			// Stamp it, as drainFile stamps its own rotation: the claim is exclusive
			// only while the MTIME says a drain holds the file. Rename preserves the
			// mtime and the claimed name still contains ".flushing.", so without
			// this the next concurrent walk -- routine, with three lane daemons on
			// one directory -- reclaims it mid-drain and delivers every line twice.
			// Nothing downstream dedupes; see this file's header. `born` was read
			// before the rename, so retirement's clock is untouched.
			reclaimedAt := time.Now()
			_ = os.Chtimes(claimed, reclaimedAt, reclaimedAt)
			// An orphan's own mtime IS its age; it is what qualified it above.
			n, err = s.drainRotated(ctx, orphanBasePath(s.Dir, name), claimed, fn, info.ModTime())
		case strings.HasSuffix(name, ".jsonl"):
			// Until empty: a sweep is a drain, and the window does not care who opened it.
			n, err = s.drainFileUntilEmpty(ctx, filepath.Join(s.Dir, name), fn)
		default:
			continue
		}
		total += n
		errs = append(errs, err)
	}
	// One session's spool failing must not hide the next one's; errors.Join
	// drops the nils, so a clean pass still returns nil.
	return total, errors.Join(errs...)
}

// drainFile rotates the session's spool aside and drains the rotated copy.
//
// The lock covers the rename and nothing else. Holding it across delivery would
// put a network round trip between a hook and its own Append, which is exactly
// what INV-3 forbids; and it is not needed, because once the file is renamed no
// later Append can reach it.
func (s Spool) drainFile(ctx context.Context, path string, fn FlushFunc) (int, error) {
	rotated := path + ".flushing." + rand.Text()
	// born is how long this data has already waited, which the rotate is about to
	// erase: the Chtimes below must stamp the rotated file `now` so a concurrent
	// sweep cannot reclaim a live drain as an orphan, and a carry-over is a
	// brand-new file. Unless it is carried forward, a churning lineage is reborn
	// every pass and the retention age never comes due.
	var born time.Time
	err := func() error {
		unlock := s.lockSpool()
		defer unlock()
		if fi, statErr := os.Stat(path); statErr == nil {
			born = fi.ModTime()
		}
		if renameErr := os.Rename(path, rotated); renameErr != nil {
			return renameErr
		}
		// Inside the lock, not after it. Between an unlock and a later stamp the
		// rotated file still carries the SOURCE's mtime -- and a carry-over file's
		// is deliberately stamped back to `born` -- so a concurrent walk landing in
		// that window sees a brand-new rotation as an orphan older than
		// ReclaimOrphanAfter and drains it in parallel with the drain that made it.
		rotatedAt := time.Now()
		_ = os.Chtimes(rotated, rotatedAt, rotatedAt)
		return nil
	}()
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // already drained / nothing spooled
		}
		return 0, fmt.Errorf("spool rotate: %w", err)
	}
	return s.drainRotated(ctx, path, rotated, fn, born)
}

func (s Spool) drainFileUntilEmpty(ctx context.Context, path string, fn FlushFunc) (int, error) {
	total := 0
	for pass := 1; pass <= MaxDrainPasses; pass++ {
		n, err := s.drainFile(ctx, path, fn)
		total += n
		if err != nil {
			return total, err
		}
		remaining := countLines(path)
		if remaining == 0 {
			return total, nil
		}
		if pass == MaxDrainPasses {
			s.logf("spool: %s: %d event(s) remain after %d drain passes; they stay spooled for the next sweep",
				filepath.Base(path), remaining, MaxDrainPasses)
		}
	}
	return total, nil
}

func (s Spool) drainRotated(ctx context.Context, basePath, file string, fn FlushFunc, born time.Time) (int, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("spool read: %w", err)
	}
	defer os.Remove(file) // best-effort; undelivered lines were re-spooled above

	attempt := RecoveryAttempt(filepath.Base(file))
	lines := NonEmptyLines(data)
	n := 0
	// refused spent an attempt; held did not. Keeping them apart is the fix: one
	// file carries one attempt number in its name, so two meanings need two files.
	var refused, held [][]byte
	// Counted rather than recorded line by line, for the same reason refused and
	// held are: one drain pass leaves one account of what it lost.
	unbuildable := 0
	// corrupt gets its OWN count for the same reason unbuildable does. It was the
	// one loss here that left no trace: not delivered, not carried, not counted,
	// and the source unlinked by the defer above -- so every counter read zero and
	// the session still said evidence_state "complete". A torn tail from a crash
	// mid-Append is exactly that shape.
	corrupt := 0
	var stopped error
	for i, line := range lines {
		if ctx.Err() != nil {
			if i == 0 {
				// Nothing on this file was even attempted, so nothing about it can
				// advance. Said out loud, because a file that is always starved this
				// way is indistinguishable from one that is refused every pass.
				s.logf("spool: %s: the budget was spent before a single delivery was attempted; "+
					"%d event(s) carried over unchanged at attempt %d",
					filepath.Base(basePath), len(lines), attempt)
			}
			held = append(held, lines[i:]...)
			stopped = ctx.Err()
			break
		}
		var ev client.DevEvent
		if json.Unmarshal(line, &ev) != nil {
			corrupt++ // skip a corrupt line; never fail the whole drain -- but say so
			continue
		}
		switch err := fn(ctx, ev); {
		case err == nil:
			n++
		case errors.Is(err, client.ErrUnbuildable):
			// Never sent, and re-sending it verbatim cannot help, so it is dropped
			// here rather than carried -- but counted, and recorded after the loop
			// under its OWN reason: this is a defect on the client side, and filing
			// it as "past N delivery attempts" would read as server pressure and
			// send an investigation to the wrong side of the wire. Loss is
			// acceptable; loss no record survives is what this file exists to stop.
			unbuildable++
		case errors.Is(err, client.ErrRefused):
			// The server judged THIS event and said no: the only thing that may
			// spend one of its attempts.
			refused = append(refused, line)
		default:
			// A transport fault, an exhausted 5xx, a 401 (which core also answers
			// for a datastore failure) or a budget that expired inside the POST --
			// Emit flattens that last cause to text, so errors.Is can never see it.
			// None of these judged the event, so none may cost it an attempt.
			held = append(held, line)
		}
	}
	// A budget that dies inside the LAST delivery leaves by the door, not the
	// break, and the caller still has to hear the pass was cut short.
	if stopped == nil {
		stopped = ctx.Err()
	}
	if unbuildable > 0 {
		s.recordDiscard(basePath, unbuildable, "the client could not build the event for delivery")
	}
	if corrupt > 0 {
		// Its own reason, so an investigation goes to the writer rather than to the
		// wire: a line that will not parse was damaged on disk, not refused.
		s.recordDiscard(basePath, corrupt, "the spooled line was not valid JSON and could not be read back")
	}
	s.carryOver(basePath, refused, held, attempt, born)
	return n, stopped
}

// carryOver splits what a drain leaves behind by whether a delivery attempt was
// actually spent on it. A refused line advances: without that, a file bigger
// than one budget never finishes its loop, the count never advances, and a
// permanently-rejected backlog churns forever at one attempt, a whole budget per
// pass. A line the budget never reached keeps its number: advancing it turns the
// retry cap into a timer that deletes any backlog too big for one budget.
//
// Two files, not one, because the number lives in the filename. held is written
// FIRST, so a second write that fails costs the half already tried rather than
// the half that never was. Both inherit born: a lineage that keeps being
// rewritten would otherwise be young forever and never come due for retirement.
func (s Spool) carryOver(basePath string, refused, held [][]byte, attempt int, born time.Time) {
	s.writeRecovery(basePath, held, attempt, born)
	s.writeRecovery(basePath, refused, attempt+1, born)
}

// MaxRecoveryAttempts bounds how many drains a line may survive undelivered.
// Without a cap, an event the server will never accept; malformed in a way the
// client cannot see, or referencing a deleted agent; would be retried on every
// flush forever, and each retry costs a request on a developer's machine.
const MaxRecoveryAttempts = 5

// ReclaimOrphanAfter bounds "no drain could still hold this"; drains take seconds.
const ReclaimOrphanAfter = 5 * time.Minute

// RecoveryAttempt reads the attempt count encoded in a recovery filename
// (`<session>.rec<N>-<id>.jsonl`). A spool or orphan file that has never been
// carried over yields 0.
func RecoveryAttempt(name string) int {
	i := strings.Index(name, ".rec")
	if i < 0 {
		return 0
	}
	rest := name[i+len(".rec"):]
	j := strings.Index(rest, "-")
	if j <= 0 {
		return 0
	}
	attempt, err := strconv.Atoi(rest[:j])
	if err != nil || attempt < 0 {
		return 0
	}
	return attempt
}

// UndeliveredCount reports how many spooled events are currently waiting in
// carry-over (recovery) files; evidence an earlier flush failed to deliver.
// Best-effort; an unreadable directory reports 0, because this feeds a
// telemetry field and must never fail a session.
func (s Spool) UndeliveredCount() int {
	return s.sumLines(IsRecoveryFile)
}

// UndeliveredCountFor is UndeliveredCount narrowed to one session: it counts
// only carry-over files whose name starts with that session's sanitized
// prefix, so another session's backlog sitting in the same directory never
// inflates this one's count. Best-effort; an unreadable directory reports 0,
// for the same reason UndeliveredCount does: this feeds a telemetry field and
// must never fail a session.
func (s Spool) UndeliveredCountFor(sessionID string) int {
	prefix := sanitizeSessionID(sessionID) + ".rec"
	return s.sumLines(func(n string) bool {
		return strings.HasPrefix(n, prefix) && IsRecoveryFile(n)
	})
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

// recoveryStem returns the canonical `<dir>/<session>` stem for a recovery
// filename, dropping any `.rec<N>-<id>` segment the input already carries.
func recoveryStem(basePath string) string {
	dir, name := filepath.Split(basePath)
	name = strings.TrimSuffix(name, ".jsonl")
	if i := strings.Index(name, ".rec"); i >= 0 {
		name = name[:i]
	}
	return filepath.Join(dir, name)
}

// writeRecovery best-effort (observe): a write failure only loses telemetry,
// never blocks anything.
func (s Spool) writeRecovery(basePath string, lines [][]byte, next int, born time.Time) {
	if len(lines) == 0 {
		return
	}
	if next < 1 {
		next = 1
	}
	if next > MaxRecoveryAttempts {
		// The bound is right; discarding in silence was not.
		s.recordDiscard(basePath, len(lines),
			fmt.Sprintf("past %d delivery attempts", MaxRecoveryAttempts))
		return
	}
	stem := recoveryStem(basePath) + ".rec" + strconv.Itoa(next) + "-" + rand.Text()
	var buf bytes.Buffer
	for _, l := range lines {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	// Written under a name no sweep matches, then renamed in. Writing straight to
	// the `.jsonl` name publishes it to FlushAll as soon as the entry appears, so
	// a sweep could read a prefix, skip the torn tail as corrupt, remove the
	// file, and the rest would follow into the removed inode.
	tmp := stem + ".partial"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		_ = os.Remove(tmp)
		s.recordDiscard(basePath, len(lines),
			fmt.Sprintf("the carry-over file could not be written: %v", err))
		return
	}
	published := stem + ".jsonl"
	if err := os.Rename(tmp, published); err != nil {
		_ = os.Remove(tmp)
		s.recordDiscard(basePath, len(lines),
			fmt.Sprintf("the carry-over file could not be renamed in: %v", err))
		return
	}
	// The data is as old as it ever was; only the file is new. Retirement reads
	// this, and nothing else does -- the orphan-reclaim clock lives on
	// `.flushing.` names, which this never touches.
	if !born.IsZero() {
		_ = os.Chtimes(published, born, born)
	}
}

// NonEmptyLines the returned slice is the caller's to index -- drainRotated
// carries `lines[i:]` into a recovery file on cancellation -- so the signature
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

func orphanBasePath(dir, name string) string {
	if i := strings.Index(name, ".flushing."); i >= 0 {
		name = name[:i]
	}
	return filepath.Join(dir, name)
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
