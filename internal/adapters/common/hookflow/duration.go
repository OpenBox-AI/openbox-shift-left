package hookflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// DurationStash is the cross-process bridge that lets a PostToolUse
// (completed) hook recover the wall-clock start time recorded by the paired
// PreToolUse (started) hook, so the completed span carries a real duration
// instead of 0. It also carries the started half's operation id forward (see
// pairRecord, pairKey), so a completed event whose own mapped arguments
// differ from the started one's -- true for an MCP call whose Pre and Post
// hook processes can each see a different tool_input -- can adopt the started
// identity instead of tearing the call's activity_id in two.
type DurationStash struct {
	Dir string // stash root; per-session subdirs live under it
}

// PutStart records a tool call's start timestamp under the session, keyed by
// the pairing key. Atomic (temp + rename) so a concurrent completed read never
// sees a partial file.
//
// Superseded by putPair for anything ThreadDuration itself writes; kept
// unchanged because it is part of this type's public surface.
func (d DurationStash) PutStart(sessionID, key, startedAt string) error {
	if startedAt == "" {
		return nil
	}
	return d.putRaw(sessionID, key, []byte(startedAt))
}

// TakeStart reads and removes the start timestamp for a pairing key, returning
// "" when none was recorded (unpaired completed, or the started record was
// lost). Kept unchanged for the same reason as PutStart.
func (d DurationStash) TakeStart(sessionID, key string) string {
	data, ok := d.takeRaw(sessionID, key)
	if !ok {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// modelCallKey is the stash key of one model-call gate: its request id, which
// both halves carry and no tool call's pairing key can equal.
func modelCallKey(requestID string) string { return "model_call\x1f" + requestID }

// PutModelCallStart records a model-call gate's start timestamp under the
// session, keyed by the gate's request id, so the finished half (a separate
// hook process) can recover it. Atomic and a no-op on an empty start or
// request id, like PutStart.
func (d DurationStash) PutModelCallStart(sessionID, requestID, startedAt string) error {
	if requestID == "" {
		return nil
	}
	return d.PutStart(sessionID, modelCallKey(requestID), startedAt)
}

// TakeModelCallStart reads and removes the start timestamp PutModelCallStart
// recorded, or "" when the started half never recorded one (a gate that never
// ran, or a lost record): the finished row then carries no duration, never an
// invented one.
func (d DurationStash) TakeModelCallStart(sessionID, requestID string) string {
	if requestID == "" {
		return ""
	}
	return d.TakeStart(sessionID, modelCallKey(requestID))
}

// pairRecord is what a started (ToolCall) event stashes for its paired
// completed (ToolResult) event to recover: the wall-clock start time, and the
// started half's own operation id. Post adopts OperationID when its own
// mapping disagrees with it: activity_id derives from operation id, never
// from the invocation id (span.invocation_id, this stash's key), because an
// approval must survive a retry that mints a fresh invocation id but repeats
// the same arguments.
//
// JSON on disk. A stash written before this type existed holds a bare RFC3339
// timestamp instead; takePair falls back to reading it that way (empty
// OperationID) so a session straddling a mid-call upgrade still recovers its
// duration.
type pairRecord struct {
	StartedAt   string `json:"started_at"`
	OperationID string `json:"operation_id,omitempty"`
}

// putPair records a started event's pair record, under the same atomicity and
// no-op-on-empty-start contract as PutStart.
func (d DurationStash) putPair(sessionID, key string, rec pairRecord) error {
	if rec.StartedAt == "" {
		return nil
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return d.putRaw(sessionID, key, body)
}

// takePair reads and removes the pair record for a key, returning the zero
// value when none was recorded. A body that fails to parse as JSON is read as
// a pre-upgrade bare timestamp rather than a lost record.
func (d DurationStash) takePair(sessionID, key string) pairRecord {
	data, ok := d.takeRaw(sessionID, key)
	if !ok {
		return pairRecord{}
	}
	var rec pairRecord
	if err := json.Unmarshal(data, &rec); err == nil {
		return rec
	}
	return pairRecord{StartedAt: strings.TrimSpace(string(data))}
}

// putRaw is PutStart and putPair's shared write.
func (d DurationStash) putRaw(sessionID, key string, body []byte) error {
	if d.Dir == "" {
		return nil
	}
	dir := d.SessionDir(sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return atomicWriteFile(d.RecordPath(sessionID, key), body, 0o600)
}

// takeRaw is TakeStart and takePair's shared read: it removes the record on a
// hit (best-effort; a leftover is swept by ClearSession at SessionEnd) and
// reports a miss with ok==false, so a caller never confuses "found, empty"
// with "not found".
func (d DurationStash) takeRaw(sessionID, key string) (data []byte, ok bool) {
	if d.Dir == "" {
		return nil, false
	}
	p := d.RecordPath(sessionID, key)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	os.Remove(p)
	return data, true
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
// (ToolCall) and completed (ToolResult) events and distinct across calls. It
// is pairKey's fallback for an event with no invocation id, and still
// includes OperationID -- so two such calls whose arguments differ still
// don't collide, but two arg-less calls of the same tool still do (a known,
// documented degradation; see docs/mapping.md).
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

// pairKey is the stash key ThreadDuration bridges a tool call's started and
// completed events under: the call's invocation id (Claude Code's and
// Codex's tool_use_id) when the event carries one, because it stays stable
// across the two hook processes even when an MCP call's mapped arguments do
// not -- and a retry mints a FRESH invocation id, so it never collides with
// the call it retries. Falls back to ToolCallStartKey for an event with no
// invocation id at all.
//
// Never the approval key: activity_id stays derived from operation id alone
// (activityPairKey, client/payload.go), which this function never touches.
func pairKey(ev client.DevEvent) string {
	if ev.Span == nil || ev.Span.InvocationID == "" {
		return ToolCallStartKey(ev)
	}
	const sep = 0x1f
	var b strings.Builder
	b.WriteString(ev.SessionID)
	b.WriteByte(sep)
	b.WriteString(ev.Span.InvocationID)
	return b.String()
}
