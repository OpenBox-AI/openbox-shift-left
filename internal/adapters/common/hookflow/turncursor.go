package hookflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// TurnCursor is the cross-process state that makes per-turn usage extraction
// count each turn exactly once.
//   - The record carries an offset AND a turn index, because the index is what
//     the activity_id is derived from, so the two must advance together or the
//     id and the window disagree;
//   - The key is (session, agent), not just session.
//   - INV-2: the record holds an integer offset, an integer index, and nothing
//     else.
type TurnCursor struct {
	Dir string // cursor root; per-session subdirs live under it
}

// TurnPos is how far a transcript has been consumed for one (session, agent)
// window: the byte offset of the first unread byte, and the index the next
// turn will take. Offset and Index are deliberately one record rather than two
// files.
type TurnPos struct {
	Offset int64 `json:"offset"`
	Index  int   `json:"index"`
}

// Read returns the recorded position for a (session, agent) window.
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
	return atomicWriteFile(c.RecordPath(sessionID, agentID), raw, 0o600)
}

// ClearSession removes a session's whole cursor subdir, sweeping the main
// thread's cursor and every subagent's in one call.
func (c TurnCursor) ClearSession(sessionID string) {
	if c.Dir == "" {
		return
	}
	os.RemoveAll(c.SessionDir(sessionID))
}

func (c TurnCursor) SessionDir(sessionID string) string {
	return filepath.Join(c.Dir, sanitizeSessionID(sessionID))
}

// RecordPath is the cursor file for one (session, agent) window.
func (c TurnCursor) RecordPath(sessionID, agentID string) string {
	return filepath.Join(c.SessionDir(sessionID), cursorKey(agentID))
}

// cursorKey the main thread gets a fixed name; a subagent gets its sanitized
// id, prefixed so it can never collide with the main thread's name whatever
// the provider mints.
func cursorKey(agentID string) string {
	if strings.TrimSpace(agentID) == "" {
		return "main.json"
	}
	return "agent-" + sanitizeSessionID(agentID) + ".json"
}
