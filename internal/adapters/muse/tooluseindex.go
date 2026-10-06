package muse

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// The tool-use index: tool_use_id -> the governed session that call belongs to.
// A Muse tool call's shell carries MUSE_TOOL_USE_ID and no session variable, and
// git's post-commit hook runs in that shell's environment, so this index is the
// only way a commit made there is tied to its session. PreToolUse writes it
// (after the subagent fold, so a subagent's call records the parent session its
// hooks re-key to) before the gate decides, and the git hook reads it.
//
// It holds ids only, never content, so it is written whatever the capture
// posture is. Entries are 0600 under a 0700 directory, written atomically, expire
// after toolUseTTL and are swept on each write.

const (
	toolUseDirName = "tooluse"
	// toolUseTTL outlives any single tool call by a wide margin: an entry older
	// than this belongs to a call that finished long ago.
	toolUseTTL = 24 * time.Hour
	// maxToolUseIDLen bounds an id read from a payload or the environment.
	maxToolUseIDLen = 256
)

type toolUseEntry struct {
	SessionID string    `json:"session_id"`
	WrittenAt time.Time `json:"written_at"`
}

func toolUseDir(spoolDir string) string {
	return filepath.Join(spoolDir, stashDirName, toolUseDirName)
}

func usableToolUseID(id string) bool { return id != "" && len(id) <= maxToolUseIDLen }

// PutToolUse records that toolUseID belongs to sessionID. An empty spool, an
// unusable id or a session id that could not name a directory is a quiet no-op:
// the index is bookkeeping, and a miss only means the commit is unattributed.
func PutToolUse(spoolDir, toolUseID, sessionID string) error {
	if spoolDir == "" || !usableToolUseID(toolUseID) || !safeSessionID(sessionID) {
		return nil
	}
	dir := toolUseDir(spoolDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sweepDir(dir, toolUseTTL)
	data, err := json.Marshal(toolUseEntry{SessionID: sessionID, WrittenAt: stashNow()})
	if err != nil {
		return err
	}
	return hookflow.AtomicWriteFile(filepath.Join(dir, stashHash(toolUseID)), data, 0o600)
}

// LookupToolUse returns the session a tool-use id was recorded under. It reads
// without deleting: one tool call may run several commits. An expired, corrupt
// or unsafe entry is a miss.
func LookupToolUse(spoolDir, toolUseID string) (string, bool) {
	if spoolDir == "" || !usableToolUseID(toolUseID) {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(toolUseDir(spoolDir), stashHash(toolUseID)))
	if err != nil {
		return "", false
	}
	var e toolUseEntry
	if json.Unmarshal(data, &e) != nil || !safeSessionID(e.SessionID) || stashNow().Sub(e.WrittenAt) > toolUseTTL {
		return "", false
	}
	return e.SessionID, true
}
