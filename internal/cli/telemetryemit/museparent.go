package telemetryemit

import (
	"encoding/json"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MuseParentOf reads the fold decision Muse's hook path records for each child
// session, so the telemetry lane puts a child's model calls in the same session
// as that child's hook rows. The hook path writes one small file per child under
// <spoolDir>/lifecycle/subagents/ (spoolDir is the muse spool, the same
// directory the daemon's unit hands it), positive or negative: a parent id when
// its link was found, none when it was not.
//
// A missing, unreadable or negative record answers "": the hook path's own
// default for a child it has not linked is a session of its own. The decision
// is made on the child's first hook, which runs before the child's first model
// call is exported, so a record arriving without one is a child no hook has
// seen.
//
// The file layout is the hook path's (internal/adapters/muse/subagentparent.go
// and runlifecycle.go statePath); this is a reader of that format and the
// golden name in the test is what notices drift.
func MuseParentOf(spoolDir string) func(child string) string {
	if spoolDir == "" {
		return func(string) string { return "" }
	}
	return func(child string) string {
		raw, err := os.ReadFile(museLinkPath(spoolDir, child))
		if err != nil {
			return ""
		}
		var link struct {
			Child  string `json:"child_session_id"`
			Parent string `json:"parent_session_id"`
		}
		if json.Unmarshal(raw, &link) != nil || link.Child != child {
			return ""
		}
		return link.Parent
	}
}

func museLinkPath(spoolDir, child string) string {
	var b strings.Builder
	for _, r := range child {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(child))
	name := b.String() + "-" + strconv.FormatUint(uint64(h.Sum32()), 16) + ".json"
	return filepath.Join(spoolDir, "lifecycle", "subagents", name)
}
