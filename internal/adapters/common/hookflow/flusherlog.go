package hookflow

import (
	"os"
	"path/filepath"
)

// FlusherLogName is where a spawned flusher's stdio goes; discarding it made a
// dead flusher indistinguishable from one never spawned.
const FlusherLogName = "flusher.log"

const maxFlusherLogBytes = 1 << 20

func (s Spool) FlusherLogPath() string {
	return filepath.Join(s.Dir, FlusherLogName)
}

func (s Spool) openFlusherLog() *os.File {
	return s.openCappedLog(s.FlusherLogPath(), maxFlusherLogBytes)
}

// openCappedLog opens a size-bounded append log, restarting it at the cap rather
// than trimming mid-line. Shared by the two logs kept beside the spool.
func (s Spool) openCappedLog(path string, maxBytes int64) *os.File {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil
	}
	if info, err := os.Stat(path); err == nil && info.Size() > maxBytes {
		_ = os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return f
}
