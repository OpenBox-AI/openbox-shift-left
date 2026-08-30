package hookflow

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// DurationStash is the cross-process bridge that lets a PostToolUse
// (completed) hook recover the wall-clock start time recorded by the paired
// PreToolUse (started) hook, so the completed span carries a real duration
// instead of 0.
type DurationStash struct {
	Dir string // stash root; per-session subdirs live under it
}

// PutStart records a tool call's start timestamp under the session, keyed by
// the pairing key. Atomic (temp + rename) so a concurrent completed read never
// sees a partial file.
func (d DurationStash) PutStart(sessionID, key, startedAt string) error {
	if d.Dir == "" || startedAt == "" {
		return nil
	}
	dir := d.SessionDir(sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return atomicWriteFile(d.RecordPath(sessionID, key), []byte(startedAt), 0o600)
}

// TakeStart reads and removes the start timestamp for a pairing key, returning
// "" when none was recorded (unpaired completed, or the started record was
// lost).
func (d DurationStash) TakeStart(sessionID, key string) string {
	if d.Dir == "" {
		return ""
	}
	p := d.RecordPath(sessionID, key)
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	os.Remove(p) // best-effort; a leftover is swept by ClearSession at SessionEnd
	return strings.TrimSpace(string(data))
}

// ClearSession removes a session's whole stash subdir (the adapter's
// SessionEnd), sweeping any records whose PostToolUse never fired.
func (d DurationStash) ClearSession(sessionID string) {
	if d.Dir == "" {
		return
	}
	os.RemoveAll(d.SessionDir(sessionID))
}

func (d DurationStash) SessionDir(sessionID string) string {
	return filepath.Join(d.Dir, sanitizeSessionID(sessionID))
}

func (d DurationStash) RecordPath(sessionID, key string) string {
	return filepath.Join(d.SessionDir(sessionID), keyHash(key))
}

func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

// ToolCallStartKey is the string identical for a tool call's started
// (ToolCall) and completed (ToolResult) events and distinct across calls.
func ToolCallStartKey(ev client.DevEvent) string {
	const sep = 0x1f
	var b strings.Builder
	b.WriteString(ev.SessionID)
	b.WriteByte(sep)
	b.WriteString(ev.Tool.Name)
	if ev.Span != nil {
		b.WriteByte(sep)
		b.WriteString(ev.Span.FilePath)
		b.WriteByte(sep)
		b.WriteString(ev.Span.Function)
		b.WriteByte(sep)
		b.WriteString(ev.Span.OperationID)
		b.WriteByte(sep)
		b.WriteString(ev.Span.InvocationID)
	}
	return b.String()
}
