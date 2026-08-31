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

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// Spool decouples the tool-call hot path from the network.
//
// The control plane does not dedupe developer events on their id, so anything
// that reaches it outside this spool has to avoid duplicating itself on its own.
// That is the rule the double-store bug broke.
type Spool struct {
	Dir string
}

// Append writes one event as a single JSON line to the session's spool file. A
// single write of a small line is atomic under O_APPEND (posix), so concurrent
// hook processes for the same session never interleave.
func (s Spool) Append(ev client.DevEvent) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("spool mkdir: %w", err)
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("spool marshal: %w", err)
	}
	line = append(line, '\n')

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
			claimed := filepath.Join(s.Dir, name) + ".reclaim." + rand.Text()
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
		errs = append(errs, err)
	}
	// One session's spool failing must not hide the next one's; errors.Join
	// drops the nils, so a clean pass still returns nil.
	return total, errors.Join(errs...)
}

func (s Spool) drainFile(ctx context.Context, path string, fn FlushFunc) (int, error) {
	rotated := path + ".flushing." + rand.Text()
	if err := os.Rename(path, rotated); err != nil {
		if os.IsNotExist(err) {
			return 0, nil // already drained / nothing spooled
		}
		return 0, fmt.Errorf("spool rotate: %w", err)
	}
	return s.drainRotated(ctx, path, rotated, fn)
}

func (s Spool) drainRotated(ctx context.Context, basePath, file string, fn FlushFunc) (int, error) {
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
	var undelivered [][]byte
	for i, line := range lines {
		if ctx.Err() != nil {
			s.writeRecovery(basePath, append(undelivered, lines[i:]...), attempt)
			return n, ctx.Err()
		}
		var ev client.DevEvent
		if json.Unmarshal(line, &ev) != nil {
			continue // skip a corrupt line; never fail the whole drain
		}
		if err := fn(ctx, ev); err != nil {
			if errors.Is(err, client.ErrUnbuildable) {
				continue
			}
			undelivered = append(undelivered, line)
			continue
		}
		n++
	}
	s.writeRecovery(basePath, undelivered, attempt)
	return n, nil
}

// MaxRecoveryAttempts bounds how many drains a line may survive undelivered.
// Without a cap, an event the server will never accept; malformed in a way the
// client cannot see, or referencing a deleted agent; would be retried on every
// flush forever, and each retry costs a request on a developer's machine.
const MaxRecoveryAttempts = 5

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
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return 0
	}
	total := 0
	for _, e := range entries {
		if e.IsDir() || !IsRecoveryFile(e.Name()) {
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
func (s Spool) writeRecovery(basePath string, lines [][]byte, attempt int) {
	if len(lines) == 0 {
		return
	}
	next := attempt + 1
	if next > MaxRecoveryAttempts {
		return
	}
	rec := recoveryStem(basePath) + ".rec" + strconv.Itoa(next) + "-" + rand.Text() + ".jsonl"
	var buf bytes.Buffer
	for _, l := range lines {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	_ = os.WriteFile(rec, buf.Bytes(), 0o600)
}

func NonEmptyLines(data []byte) [][]byte {
	var out [][]byte
	for _, l := range bytes.Split(data, []byte{'\n'}) {
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
