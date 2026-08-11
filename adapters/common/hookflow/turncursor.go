package hookflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// TurnCursor is the cross-process state that makes per-turn usage extraction
// count each turn exactly once.
//
// A provider fires its turn-boundary hook (Claude Code's Stop) as a separate
// short-lived process, and the only source of token usage is the session
// transcript — a file that grows. Without shared state, every firing would
// re-read the whole transcript and re-report every turn's tokens, so a session
// of N turns would report O(N²) tokens. The cursor records how far the
// transcript has been consumed and which turn index comes next, so each firing
// reads only its own window.
//
// Modelled on the findings cursor (findings.go), with two differences the turn
// case forces:
//
//   - the record carries an offset AND a turn index, because the index is what
//     the activity_id is derived from, so the two must advance together or the
//     id and the window disagree;
//   - the key is (session, agent), not just session. A subagent's Stop fires
//     against its own transcript window; sharing one cursor with the main
//     thread would interleave the two and hand each the other's tokens.
//
// Ordering rule, stated because it is the correctness argument and not a
// preference: the caller spools the events FIRST and advances the cursor
// SECOND. A crash between the two re-reads one turn on the next firing, which
// re-mints the same deterministic activity_id (<session>:turn:<n>) — and core's
// dedupe key includes activity_id and event_type, so the server returns the
// cached verdict rather than storing a second row. The reverse order loses a
// turn's tokens silently, with nothing to recover them from. Over-report into a
// server that deduplicates; never under-report into nothing.
//
// Safety:
//   - INV-2: the record holds an integer offset, an integer index, and nothing
//     else. No content, no filenames from the transcript, no model id. Same
//     posture as the duration stash.
//   - INV-3: every fault is swallowed. A missing or corrupt cursor reads as
//     zero (re-read from the start — over-report, server-deduped), and a write
//     failure leaves the old cursor in place (the window is re-read next time).
//     Nothing here can fail a hook or block a session.
//   - Session and agent ids are sanitized before use as path segments
//     (sanitizeSessionID), so a crafted id cannot escape the cursor directory.
type TurnCursor struct {
	Dir string // cursor root; per-session subdirs live under it
}

// TurnPos is how far a transcript has been consumed for one (session, agent)
// window: the byte offset of the first unread byte, and the index the next turn
// will take.
//
// Offset and Index are deliberately one record rather than two files. They are
// read together and must advance together — an offset that advanced without its
// index would re-use an activity_id for a different window, which core would
// dedupe away as a replay, silently dropping a turn's tokens.
type TurnPos struct {
	Offset int64 `json:"offset"`
	Index  int   `json:"index"`
}

// Read returns the recorded position for a (session, agent) window. A missing,
// unreadable, corrupt, or negative record yields the zero position — read the
// transcript from the start, which over-reports into a server that
// deduplicates rather than under-reporting into nothing (INV-3).
//
// agentID is "" for the main thread.
func (c TurnCursor) Read(sessionID, agentID string) TurnPos {
	if c.Dir == "" {
		return TurnPos{}
	}
	raw, err := os.ReadFile(c.RecordPath(sessionID, agentID))
	if err != nil {
		return TurnPos{}
	}
	var p TurnPos
	if err := json.Unmarshal(raw, &p); err != nil {
		return TurnPos{}
	}
	if p.Offset < 0 || p.Index < 0 {
		return TurnPos{}
	}
	return p
}

// Write records the new position atomically (temp file + rename) so a
// concurrent reader never sees a half-written record.
//
// Call it only AFTER the turn's events have been durably spooled — see the
// ordering rule on TurnCursor. Best-effort: the returned error is for the
// caller's fail-open log, and leaving the cursor unadvanced re-reads the same
// window next time (at-most-once locally, exactly-once after server dedupe).
func (c TurnCursor) Write(sessionID, agentID string, p TurnPos) error {
	if c.Dir == "" {
		return nil
	}
	dir := c.SessionDir(sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, cursorKey(agentID)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, c.RecordPath(sessionID, agentID)); err != nil {
		os.Remove(tmp) // don't leave the temp behind on a failed rename
		return err
	}
	return nil
}

// ClearSession removes a session's whole cursor subdir, sweeping the main
// thread's cursor and every subagent's in one call. Called from the
// SessionEnded sweep alongside the duration stash. Best-effort.
func (c TurnCursor) ClearSession(sessionID string) {
	if c.Dir == "" {
		return
	}
	os.RemoveAll(c.SessionDir(sessionID))
}

func (c TurnCursor) SessionDir(sessionID string) string {
	return filepath.Join(c.Dir, sanitizeSessionID(sessionID))
}

// RecordPath is the cursor file for one (session, agent) window. Both segments
// are sanitized, so neither a session nor an agent id can traverse out of Dir.
func (c TurnCursor) RecordPath(sessionID, agentID string) string {
	return filepath.Join(c.SessionDir(sessionID), cursorKey(agentID))
}

// cursorKey is the per-agent filename within a session's cursor dir. The main
// thread gets a fixed name; a subagent gets its sanitized id, prefixed so it
// can never collide with the main thread's name whatever the provider mints.
func cursorKey(agentID string) string {
	if strings.TrimSpace(agentID) == "" {
		return "main.json"
	}
	return "agent-" + sanitizeSessionID(agentID) + ".json"
}
