package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// Spool decouples the tool-call hot path from the network — a verbatim
// port of the Claude Code adapter's spool (its README documents the
// design; the spool contract is provider-independent but the code stays
// adapter-local — only devconfig was ruled into a shared module). Mapped
// events are appended to a per-session append-only JSONL file (local I/O,
// well under a <50ms budget); a bounded Flush drains them to /evaluate off
// the hot path (at SessionEnd, or via `openbox hook codex flush`).
//
// Delivery is effectively-once (fail-open, INV-3): an undelivered event is
// carried over to a recovery file and retried on a later flush, and the server
// deduplicates on the Idempotency-Key its stable id produces, so a re-send
// cannot double-count (E8-S7). A hard outage delays telemetry rather than
// losing it, and never touches a tool call. Loss becomes permanent only past
// maxRecoveryAttempts. When a flush is cut short (time budget / ctx
// cancellation), the UNDELIVERED remainder is persisted to a recovery file,
// which the next SessionEnd re-drains via SweepRecovery (or an explicit `flush`
// / FlushAll) — delivered events are never re-sent, the tail is never dropped
// (INV-5 event ids are stable through the whole lifecycle).
type Spool struct {
	Dir string
}

// Append writes one event as a single JSON line to the session's spool file.
// A single write of a small line is atomic under O_APPEND (POSIX), so
// concurrent hook processes for the same session never interleave. Errors are
// returned for the caller to log fail-open — they never propagate to Codex.
func (s Spool) Append(ev client.DevEvent) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("spool mkdir: %w", err)
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("spool marshal: %w", err)
	}
	line = append(line, '\n')

	f, err := os.OpenFile(s.sessionPath(ev.SessionID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("spool open: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("spool write: %w", err)
	}
	return nil
}

// FlushFunc delivers one spooled event. A non-nil error is logged only; it does
// not stop the drain (delivery is best-effort). The client's Emit fits this.
type FlushFunc func(context.Context, client.DevEvent) error

// FlushSession drains one session's spool through fn and returns the number of
// events delivered. It rotates the spool file first (atomic rename) so late
// appends land in a fresh file. If ctx ends the drain early, the undelivered
// remainder is persisted to a recovery file, which SweepRecovery re-drains.
func (s Spool) FlushSession(ctx context.Context, sessionID string, fn FlushFunc) (int, error) {
	return s.drainFile(ctx, s.sessionPath(sessionID), fn)
}

// SweepRecovery re-drains carry-over (recovery) files left by earlier flushes —
// any session's, not just the caller's — and returns the number of events
// delivered.
//
// This is what makes E8-S7's retry real rather than notional. A recovery file is
// written by a session that has already ended, so that session has no later
// trigger of its own: without an unowned sweep, `FlushSession` would only ever
// touch `<session>.jsonl` and every carry-over would sit on disk forever (a
// developer offline for an afternoon would accumulate one per session and report
// a degraded evidence state from then on). Sweeping by session would not fix it
// either — each new thread has a new id.
//
// Only BARE `.rec<N>-<id>.jsonl` files are claimed, never a live `<session>.jsonl`
// (which may belong to a session still running, and whose events are merely
// un-flushed rather than undelivered) and never an in-flight `.flushing.`
// rotation. That restriction is what makes an unowned sweep safe: nobody holds a
// bare recovery file open, and drainFile claims it by atomic rename, so a
// concurrent sweep from another session's teardown loses the race cleanly
// instead of double-delivering.
//
// The directory is listed once up front, so recovery files this sweep itself
// writes are not re-drained in the same pass — otherwise a single offline flush
// would burn every remaining attempt (maxRecoveryAttempts) at once and turn a
// bounded retry into immediate loss.
func (s Spool) SweepRecovery(ctx context.Context, fn FlushFunc) (int, error) {
	return s.sweepRecovery(ctx, s.recoveryFiles(), "", fn)
}

// recoveryFiles snapshots the carry-over files currently in the spool directory.
// An unreadable directory yields none: a sweep is best-effort catch-up (observe,
// INV-3), never a reason to fail a caller.
func (s Spool) recoveryFiles() []string {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && isRecoveryFile(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// sweepRecovery drains the named carry-over files, those belonging to
// ownSession (if any) first so the ending session's own telemetry gets the
// budget before other sessions' backlog does. It continues past a single file's
// error and stops on ctx expiry, leaving the rest for a later sweep.
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
	var firstErr error
	for _, name := range names {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		n, err := s.drainFile(ctx, filepath.Join(s.Dir, name), fn)
		total += n
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return total, firstErr
}

// isRecoveryFile reports whether name is a carry-over file —
// `<session>.rec<N>-<id>.jsonl`, or the legacy `<session>.rec-<id>.jsonl` written
// before the attempt counter existed.
//
// The match is on the segment's shape rather than a `.rec` substring, because
// `.reclaim.<id>` (a claimed orphan, i.e. a drain in flight) contains `.rec`
// too. Counting or claiming one of those would inflate the undelivered count
// with a file that is being actively delivered.
func isRecoveryFile(name string) bool {
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

// FlushAll drains every session spool in the directory — the `flush`
// subcommand path — INCLUDING recovery files (`*.rec-*.jsonl`) left by a
// budget-bounded flush and orphaned `*.flushing.*` files left by a drain whose
// process was killed mid-flight. It continues past a single file's error.
func (s Spool) FlushAll(ctx context.Context, fn FlushFunc) (int, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // nothing spooled yet
		}
		return 0, fmt.Errorf("spool readdir: %w", err)
	}
	total := 0
	var firstErr error
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
			// Orphan from a killed drain: re-claim it atomically, then drain.
			// (Re-delivering its already-sent prefix is possible on this rare
			// path; event_id dedupe handles it once EXT-core lands.)
			claimed := filepath.Join(s.Dir, name) + ".reclaim." + randomID()
			if os.Rename(filepath.Join(s.Dir, name), claimed) != nil {
				continue // lost the race to another drain, or already gone
			}
			n, err = s.drainRotated(ctx, orphanBasePath(s.Dir, name), claimed, fn)
		case strings.HasSuffix(name, ".jsonl"):
			n, err = s.drainFile(ctx, filepath.Join(s.Dir, name), fn)
		default:
			continue
		}
		total += n
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return total, firstErr
}

// drainFile rotates path to a unique .flushing file, then drains it.
func (s Spool) drainFile(ctx context.Context, path string, fn FlushFunc) (int, error) {
	rotated := path + ".flushing." + randomID()
	if err := os.Rename(path, rotated); err != nil {
		if os.IsNotExist(err) {
			return 0, nil // already drained / nothing spooled
		}
		return 0, fmt.Errorf("spool rotate: %w", err)
	}
	return s.drainRotated(ctx, path, rotated, fn)
}

// drainRotated delivers every event in a rotated spool file through fn, then
// removes it. The whole (small) file is read up front so the undelivered
// remainder can be persisted precisely: on ctx cancellation it writes the
// not-yet-delivered lines to a recovery file and stops — delivered lines are
// never rewritten (a delivered event is never re-sent), the tail is never lost.
func (s Spool) drainRotated(ctx context.Context, basePath, file string, fn FlushFunc) (int, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("spool read: %w", err)
	}
	defer os.Remove(file) // best-effort; undelivered lines were re-spooled above

	attempt := recoveryAttempt(filepath.Base(file))
	lines := nonEmptyLines(data)
	n := 0
	var undelivered [][]byte
	for i, line := range lines {
		if ctx.Err() != nil {
			// Budget/ctx exhausted: the not-yet-tried tail plus anything that
			// failed so far carries over.
			s.writeRecovery(basePath, append(undelivered, lines[i:]...), attempt)
			return n, ctx.Err()
		}
		var ev client.DevEvent
		if json.Unmarshal(line, &ev) != nil {
			continue // skip a corrupt line; never fail the whole drain
		}
		if err := fn(ctx, ev); err != nil {
			// A delivery error used to end the event's life here, which made
			// at-most-once a data-loss guarantee rather than a safety one. The
			// line is now carried over to a recovery file and retried, which is
			// safe because the server deduplicates on the Idempotency-Key this
			// event's stable id produces (E8-S7): re-sending an event that did
			// land returns the original verdict instead of counting it twice.
			undelivered = append(undelivered, line)
			continue
		}
		n++
	}
	s.writeRecovery(basePath, undelivered, attempt)
	return n, nil
}

// maxRecoveryAttempts bounds how many drains a line may survive undelivered.
//
// Without a cap, an event the server will never accept — malformed in a way the
// client cannot see, or referencing a deleted agent — would be retried on every
// flush forever, and each retry costs a request on a developer's machine. After
// the cap the line is dropped and logged: still fail-open (INV-3), and bounded
// loss beats an unbounded loop.
const maxRecoveryAttempts = 5

// recoveryAttempt reads the attempt count encoded in a recovery filename
// (`<session>.rec<N>-<id>.jsonl`). A spool or orphan file that has never been
// carried over yields 0. Legacy `.rec-<id>.jsonl` names (written before the
// counter existed) also read as 0, so they get a full allowance rather than
// being dropped on sight.
func recoveryAttempt(name string) int {
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
// carry-over (recovery) files — evidence an earlier flush failed to deliver.
//
// It counts only `.rec<N>-*` files, not the live session spool: the live file
// holds events that have simply not been flushed yet, which is normal and not a
// degradation. Best-effort — an unreadable directory reports 0, because this
// feeds a telemetry field and must never fail a session.
func (s Spool) UndeliveredCount() int {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return 0
	}
	total := 0
	for _, e := range entries {
		if e.IsDir() || !isRecoveryFile(e.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			continue
		}
		total += len(nonEmptyLines(data))
	}
	return total
}

// recoveryStem returns the canonical `<dir>/<session>` stem for a recovery
// filename, dropping any `.rec<N>-<id>` segment the input already carries.
// Without this, each carry-over appended another segment: the name grew until
// the filesystem rejected it, and recoveryAttempt — which reads the FIRST
// segment — kept seeing the original attempt, so the retry bound never engaged.
func recoveryStem(basePath string) string {
	dir, name := filepath.Split(basePath)
	name = strings.TrimSuffix(name, ".jsonl")
	if i := strings.Index(name, ".rec"); i >= 0 {
		name = name[:i]
	}
	return filepath.Join(dir, name)
}

// writeRecovery persists undelivered lines to a fresh
// `<session>.rec<N>-<id>.jsonl` file that FlushAll re-drains later, where N is
// one more than the attempt this drain was. Best-effort (observe): a write
// failure only loses telemetry, never blocks anything.
func (s Spool) writeRecovery(basePath string, lines [][]byte, attempt int) {
	if len(lines) == 0 {
		return
	}
	next := attempt + 1
	if next > maxRecoveryAttempts {
		// Bounded give-up: see maxRecoveryAttempts. Counted, not silent — the
		// caller's evidence_state reports a degraded session.
		return
	}
	rec := recoveryStem(basePath) + ".rec" + strconv.Itoa(next) + "-" + randomID() + ".jsonl"
	var buf bytes.Buffer
	for _, l := range lines {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	_ = os.WriteFile(rec, buf.Bytes(), 0o600)
}

func nonEmptyLines(data []byte) [][]byte {
	var out [][]byte
	for _, l := range bytes.Split(data, []byte{'\n'}) {
		if len(l) > 0 {
			out = append(out, l)
		}
	}
	return out
}

// orphanBasePath recovers the canonical .jsonl base path from an orphaned
// `<session>.jsonl.flushing.<id>` filename, for recovery-file naming.
func orphanBasePath(dir, name string) string {
	if i := strings.Index(name, ".flushing."); i >= 0 {
		name = name[:i]
	}
	return filepath.Join(dir, name)
}

// sessionPath is the spool file for a session id, sanitized for the filesystem.
func (s Spool) sessionPath(sessionID string) string {
	return filepath.Join(s.Dir, sanitizeSessionID(sessionID)+".jsonl")
}

// sanitizeSessionID reduces a session id to a safe filename component. Codex
// thread ids are UUIDs, but be defensive against unexpected input.
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
