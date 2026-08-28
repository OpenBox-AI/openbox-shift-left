package hookflow

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// DurationStash is the cross-process bridge that lets a PostToolUse
// (completed) hook recover the wall-clock start time recorded by the paired
// PreToolUse (started) hook, so the completed span carries a real duration
// instead of 0. Claude Code fires PreToolUse/PostToolUse as separate
// short-lived processes, so without shared state the completed span's
// start_time would be its own timestamp.
//
// Mirrors the session registry (adapters/common/git/registry.go): the
// started hook writes a tiny record keyed by the tool call's pairing key; the
// completed hook reads+deletes it and stamps ev.StartedAt before the event is
// spooled, so pairing survives the spool's rotation/recovery-file splitting.
// The client (client/payload.go durationMs) then computes duration_ms =
// end - start, and OMITS the field when this recovery missed.
//
// Records carry only a structural RFC3339 timestamp (INV-2: no content); the
// filename is a hash of structural locators (session, tool name, file/
// function), never content. Best-effort throughout (INV-3): any fault only
// costs duration accuracy — never a tool call, never the event itself.
type DurationStash struct {
	Dir string // stash root; per-session subdirs live under it
}

// PutStart records a tool call's start timestamp under the session, keyed by the
// pairing key. Atomic (temp + rename) so a concurrent completed read never sees a
// partial file. Best-effort: an error is returned for the caller to ignore.
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

// TakeStart reads and removes the start timestamp for a pairing key, returning ""
// when none was recorded (unpaired completed, or the started record was lost).
// Removing on read keeps the stash bounded in the normal Pre→Post flow.
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

// ClearSession removes a session's whole stash subdir (the adapter's SessionEnd),
// sweeping any records whose PostToolUse never fired. Best-effort.
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

// keyHash is the filename for a pairing key — a fixed-width hex digest so an
// arbitrary key (which folds in a file path) is always a safe, bounded filename.
func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

// ToolCallStartKey is the string IDENTICAL for a tool call's started (ToolCall)
// and completed (ToolResult) events and distinct across calls. It is the
// activity pair key plus the invocation id, so the recovered start time lands on
// the right completed event.
//
// It follows the INVOCATION, not the operation: two attempts at the same
// operation are one activity but two executions with two durations, and keying this
// on the operation would make the second attempt recover the first's start time
// and report a wildly inflated duration.
//
// Only structural locators and opaque ids feed it (INV-2). A drift from the
// client's key would merely reduce duration accuracy; it can never break span
// pairing, which the client derives independently at buildPayload time.
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
