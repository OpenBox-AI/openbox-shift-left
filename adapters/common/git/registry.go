package git

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Session registry — the parallel-safe bridge between an adapter (which knows
// the session id) and the prepare-commit-msg hook (which does not).
//
// The problem: Claude Code (and peers) do NOT expose the session id to the git
// subprocess — no env var, no pid the hook could walk (verified against the
// hooks docs). But the adapter's own hooks fire with `session_id` + `cwd`, and a
// `git commit` run through the Bash tool is ALWAYS immediately preceded by that
// session's PreToolUse. So the adapter writes a small liveness record per
// session, and the hook resolves the commit to the most-recently-updated session
// whose working directory lies within the commit's git worktree.
//
// Why this is parallel-safe (the multiple-concurrent-sessions requirement):
//   - Sessions in DIFFERENT worktrees never collide — the worktree filter is
//     exact, so each commit resolves to its own repo's session.
//   - Sessions in the same worktree resolve by recency: the committing
//     session refreshed its record on the PreToolUse that fired ms before
//     the commit, so it is the freshest. This is best-effort (a tight
//     interleaving race is possible) — acceptable because the git action
//     makes the authoritative binding server-side at push.
//
// Records carry only structural fields (session id, cwd, timestamp) —
// never content (INV-2); cwd is already a blessed structural field in
// the contract's schema.
const (
	EnvSessionDir = "OPENBOX_SESSION_DIR" // overrides the registry location
	EnvSessionTTL = "OPENBOX_SESSION_TTL" // staleness cutoff, in seconds
)

// defaultSessionTTL bounds how old a record may be and still be attributed. A
// live session refreshes on every hook, so this only rejects records left by a
// crashed session (no SessionEnd) so a much-later human commit is not falsely
// attributed to it. Generous, to tolerate a session that idles then commits.
const defaultSessionTTL = 8 * time.Hour

// SessionRecord is one active developer session's liveness record.
type SessionRecord struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	UpdatedAt int64  `json:"updated_at"` // unix nanoseconds (sub-second recency tiebreak)
}

// DefaultSessionDir is the shared registry location used by both the adapter
// writer and the hook resolver. OPENBOX_SESSION_DIR overrides it.
func DefaultSessionDir() string {
	if p := os.Getenv(EnvSessionDir); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "openbox", "sessions")
}

// WriteSessionRecord creates or refreshes a session's liveness record (a
// "touch"). The adapter calls it on SessionStart and each tool-use hook so the
// committing session is the freshest in its worktree at commit time. The write
// is atomic (temp + rename) so a concurrent resolver never reads a partial file.
// Best-effort: the caller logs an error fail-open (never blocks a tool call).
func WriteSessionRecord(dir, sessionID, cwd string, now time.Time) error {
	// Validate at the source too: the trailer sink already gates every
	// id, but a malformed/secret-shaped id should never even be
	// persisted to the registry. Invalid → skip silently (best-effort;
	// never blocks a hook).
	if err := validateSessionID(sessionID); err != nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(SessionRecord{SessionID: sessionID, Cwd: cwd, UpdatedAt: now.UnixNano()})
	if err != nil {
		return err
	}
	// Unique temp name: two hooks for the same session firing concurrently
	// must not clobber a shared "<id>.tmp". CreateTemp yields
	// "<id>-<rand>.tmp" (0600); the resolver ignores non-.json files.
	// Rename is atomic.
	f, err := os.CreateTemp(dir, sanitizeForFile(sessionID)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, sessionRecordPath(dir, sessionID))
}

// RemoveSessionRecord deletes a session's record (the adapter's SessionEnd).
// Absence is not an error. Best-effort.
func RemoveSessionRecord(dir, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	if err := os.Remove(sessionRecordPath(dir, sessionID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func sessionRecordPath(dir, sessionID string) string {
	return filepath.Join(dir, sanitizeForFile(sessionID)+".json")
}

// sanitizeForFile reduces a session id to a safe filename component (session ids
// are UUIDs; be defensive about unexpected input).
func sanitizeForFile(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
