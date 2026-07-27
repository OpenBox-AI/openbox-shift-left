package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// durationStash is the cross-process bridge that lets a PostToolUse
// (completed) hook recover the wall-clock start time recorded by the
// paired PreToolUse (started) hook, so the completed span carries a real
// cross-process duration instead of 0 — ported from the Claude Code
// adapter (its duration.go documents the full rationale).
//
// The Codex twist: the stash key is derived from the same field set as
// the client's activityPairKey — (session, tool name, file/function
// locator) — and for non-MCP tools the mapper places the per-invocation
// `tool_use_id` in the function slot, so the stash is effectively keyed
// by tool_use_id: two identical concurrent tool calls can no longer swap
// start times. A stash miss (record lost, unpaired completed) yields
// duration 0, never an error (INV-3).
//
// Records carry ONLY a structural RFC3339 timestamp (INV-2); the filename is a
// hash of structural identifiers, never content.
type durationStash struct {
	Dir string // stash root; per-session subdirs live under it
}

// putStart records a tool call's start timestamp under the session, keyed by
// the pairing key. Atomic (temp + rename) so a concurrent completed read never
// sees a partial file. Best-effort: an error is returned for the caller to
// ignore.
func (d durationStash) putStart(sessionID, key, startedAt string) error {
	if d.Dir == "" || startedAt == "" {
		return nil
	}
	dir := d.sessionDir(sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, keyHash(key)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.WriteString(startedAt); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, d.recordPath(sessionID, key)); err != nil {
		os.Remove(tmp) // don't leave the temp behind on a failed rename
		return err
	}
	return nil
}

// takeStart reads and removes the start timestamp for a pairing key, returning
// "" when none was recorded (unpaired completed, or the started record was
// lost). Removing on read keeps the stash bounded in the normal Pre→Post flow.
func (d durationStash) takeStart(sessionID, key string) string {
	if d.Dir == "" {
		return ""
	}
	p := d.recordPath(sessionID, key)
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	os.Remove(p) // best-effort; a leftover is swept by clearSession at SessionEnd
	return strings.TrimSpace(string(data))
}

// clearSession removes a session's whole stash subdir (the adapter's
// SessionEnd), sweeping any records whose PostToolUse never fired. Best-effort.
func (d durationStash) clearSession(sessionID string) {
	if d.Dir == "" {
		return
	}
	os.RemoveAll(d.sessionDir(sessionID))
}

func (d durationStash) sessionDir(sessionID string) string {
	return filepath.Join(d.Dir, sanitizeSessionID(sessionID))
}

func (d durationStash) recordPath(sessionID, key string) string {
	return filepath.Join(d.sessionDir(sessionID), keyHash(key))
}

// keyHash is the filename for a pairing key — a fixed-width hex digest so an
// arbitrary key is always a safe, bounded filename.
func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

// toolCallStartKey is the string IDENTICAL for a tool call's started (ToolCall)
// and completed (ToolResult) events and — via the tool_use_id the mapper puts
// in span.function for non-MCP tools — exact per invocation. It mirrors the
// field set of client.activityPairKey so the recovered start time lands on the
// same completed span the client pairs onto one span_id. Only structural
// identifiers feed it (INV-2). A drift from the client's key would merely
// reduce duration accuracy; it can never break span pairing, which the client
// derives independently at buildPayload time.
func toolCallStartKey(ev client.DevEvent) string {
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
	}
	return b.String()
}
