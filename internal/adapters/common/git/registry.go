package git

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Session registry; the parallel-safe bridge between an adapter (which knows
// the session id) and the prepare-commit-msg hook (which does not).
const (
	EnvSessionDir = "OPENBOX_SESSION_DIR" // overrides the registry location
	EnvSessionTTL = "OPENBOX_SESSION_TTL" // staleness cutoff, in seconds
)

const defaultSessionTTL = 8 * time.Hour

// SessionRecord is one active developer session's liveness record.
type SessionRecord struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	UpdatedAt int64  `json:"updated_at"` // unix nanoseconds (sub-second recency tiebreak)
}

// DefaultSessionDir is the shared registry location used by both the adapter
// writer and the hook resolver.
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
// "touch"). The write is atomic (temp + rename) so a concurrent resolver never
// reads a partial file.
func WriteSessionRecord(dir, sessionID, cwd string, now time.Time) error {
	// Invalid → skip silently (best-effort; never blocks a hook).
	if err := ValidateSessionID(sessionID); err != nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(SessionRecord{SessionID: sessionID, Cwd: cwd, UpdatedAt: now.UnixNano()})
	if err != nil {
		return err
	}
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
