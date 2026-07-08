package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// Spool decouples the tool-call hot path from the network. Mapped events are
// appended to a per-session append-only JSONL file (local I/O, well under the
// NFR-2 <50 ms budget); a bounded Flush drains them to /evaluate off the hot
// path (at SessionEnd, or via the `flush` subcommand / a CLI-driven drain).
//
// This is how observe-only stays truly non-blocking with synchronous hooks:
// nothing on the PreToolUse/PostToolUse path touches OpenBox. Delivery is
// at-most-once best-effort (fail-open, INV-3) — a hard outage loses telemetry,
// never a tool call. When a flush is cut short (its time budget or ctx
// cancellation), the UNDELIVERED remainder is persisted to a recovery file so a
// later drain (`flush` / FlushAll) completes it — delivered events are never
// re-sent (at-most-once preserved), and the tail is not dropped.
type Spool struct {
	Dir string
}

// Append writes one event as a single JSON line to the session's spool file.
// A single write of a small line is atomic under O_APPEND (POSIX), so concurrent
// hook processes for the same session never interleave. Errors are returned for
// the caller to log fail-open — they never propagate to Claude Code.
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
// remainder is persisted to a recovery file (picked up by a later FlushAll).
func (s Spool) FlushSession(ctx context.Context, sessionID string, fn FlushFunc) (int, error) {
	return s.drainFile(ctx, s.sessionPath(sessionID), fn)
}

// FlushAll drains every session spool in the directory — the `flush` subcommand
// / CLI-driven catch-up path — INCLUDING recovery files (`*.rec-*.jsonl`) left
// by a budget-bounded flush and orphaned `*.flushing.*` files left by a drain
// whose process was killed mid-flight. It continues past a single file's error.
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
// never rewritten (at-most-once), the tail is never lost. basePath is the
// session's canonical .jsonl path, used to name the recovery file.
func (s Spool) drainRotated(ctx context.Context, basePath, file string, fn FlushFunc) (int, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("spool read: %w", err)
	}
	defer os.Remove(file) // best-effort; delivered lines are gone (at-most-once)

	lines := nonEmptyLines(data)
	n := 0
	for i, line := range lines {
		if ctx.Err() != nil {
			s.writeRecovery(basePath, lines[i:]) // persist the undelivered tail
			return n, ctx.Err()
		}
		var ev client.DevEvent
		if json.Unmarshal(line, &ev) != nil {
			continue // skip a corrupt line; never fail the whole drain
		}
		_ = fn(ctx, ev) // fail-open: delivery errors are the emitter's to log
		n++
	}
	return n, nil
}

// writeRecovery persists undelivered lines to a fresh `<session>.rec-<id>.jsonl`
// file that FlushAll re-drains later. Best-effort (observe): a write failure
// only loses telemetry, never blocks anything.
func (s Spool) writeRecovery(basePath string, lines [][]byte) {
	if len(lines) == 0 {
		return
	}
	rec := strings.TrimSuffix(basePath, ".jsonl") + ".rec-" + randomID() + ".jsonl"
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

// sanitizeSessionID reduces a session id to a safe filename component. Claude
// Code session ids are UUIDs, but be defensive against unexpected input.
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
