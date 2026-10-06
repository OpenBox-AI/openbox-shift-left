package muse

import (
	"os"
	"path/filepath"
	"testing"
)

// The telemetry lane reads a child's fold through SubagentParentOf, so what the
// hook path saves is what that reader answers: a positive link folds, and a
// negative, absent or unreadable one leaves the child a session of its own.
func TestSubagentParentOfReadsWhatTheHookPathSaved(t *testing.T) {
	spool := t.TempDir()
	lc := lifecycle{Dir: lifecycleDir(spool)}
	parentOf := SubagentParentOf(spool)

	if got := parentOf("sess-0002"); got != "" {
		t.Errorf("absent link: parent %q, want none", got)
	}
	lc.saveLink(subagentLink{ChildSessionID: "sess-0002", ParentSessionID: "sess-0001", AgentID: "skill-reminder"})
	if got := parentOf("sess-0002"); got != "sess-0001" {
		t.Errorf("saved link: parent %q, want sess-0001", got)
	}
	lc.saveLink(subagentLink{ChildSessionID: "sess-0003"})
	if got := parentOf("sess-0003"); got != "" {
		t.Errorf("negative link: parent %q, want none", got)
	}
	if err := os.WriteFile(lc.linkPath("sess-0004"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := parentOf("sess-0004"); got != "" {
		t.Errorf("corrupt link: parent %q, want none", got)
	}
	if got := SubagentParentOf("")("sess-0002"); got != "" {
		t.Errorf("no spool dir: parent %q, want none", got)
	}
	if _, err := os.Stat(filepath.Join(spool, "lifecycle", subagentLinkDir)); err != nil {
		t.Errorf("the link is not where the daemon's spool dir leads: %v", err)
	}
}
