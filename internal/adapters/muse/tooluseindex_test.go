package muse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
)

func TestToolUseIndexRoundTrip(t *testing.T) {
	spool := t.TempDir()
	if err := PutToolUse(spool, "call_1", "sess-parent"); err != nil {
		t.Fatal(err)
	}
	got, ok := LookupToolUse(spool, "call_1")
	if !ok || got != "sess-parent" {
		t.Fatalf("LookupToolUse = %q, %v; want sess-parent", got, ok)
	}
	// A lookup is a read: the same id still resolves for a second commit in one call.
	if got, ok := LookupToolUse(spool, "call_1"); !ok || got != "sess-parent" {
		t.Fatalf("second lookup = %q, %v", got, ok)
	}
	if _, ok := LookupToolUse(spool, "call_2"); ok {
		t.Fatal("an unknown id resolved")
	}
}

func TestToolUseIndexFileIsPrivateAndHoldsIDsOnly(t *testing.T) {
	spool := t.TempDir()
	if err := PutToolUse(spool, "call_1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	var files []string
	_ = filepath.Walk(filepath.Join(spool, stashDirName, toolUseDirName), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			files = append(files, p)
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("%s mode = %v, want 0600", p, fi.Mode().Perm())
			}
		}
		return nil
	})
	if len(files) != 1 {
		t.Fatalf("index files = %v, want one", files)
	}
	if strings.Contains(filepath.Base(files[0]), "call_1") {
		t.Errorf("file name %q leaks the raw id", files[0])
	}
}

func TestToolUseIndexExpiresAfterTTL(t *testing.T) {
	spool := t.TempDir()
	base := time.Now()
	stashNow = func() time.Time { return base }
	t.Cleanup(func() { stashNow = time.Now })
	if err := PutToolUse(spool, "call_1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	stashNow = func() time.Time { return base.Add(toolUseTTL + time.Minute) }
	if _, ok := LookupToolUse(spool, "call_1"); ok {
		t.Fatal("an expired entry resolved")
	}
}

func TestToolUseIndexSweepsExpiredEntriesOnWrite(t *testing.T) {
	spool := t.TempDir()
	if err := PutToolUse(spool, "old", "sess-old"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(spool, stashDirName, toolUseDirName)
	entries, _ := os.ReadDir(dir)
	old := time.Now().Add(-toolUseTTL - time.Hour)
	for _, e := range entries {
		if err := os.Chtimes(filepath.Join(dir, e.Name()), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := PutToolUse(spool, "new", "sess-new"); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("entries after sweep = %d, want 1", len(entries))
	}
}

func TestToolUseIndexRefusesUnsafeInput(t *testing.T) {
	spool := t.TempDir()
	for _, tc := range []struct{ spool, id, sess string }{
		{spool, "", "s"},
		{spool, "call_1", ""},
		{spool, "call_1", "../escape"},
		{spool, "call_1", "a/b"},
		{"", "call_1", "s"},
	} {
		if err := PutToolUse(tc.spool, tc.id, tc.sess); err != nil {
			t.Errorf("PutToolUse(%q,%q,%q) = %v, want a quiet no-op", tc.spool, tc.id, tc.sess, err)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(spool, stashDirName, toolUseDirName)); len(entries) != 0 {
		t.Fatalf("unsafe input wrote %d entries", len(entries))
	}
	if _, ok := LookupToolUse(spool, ""); ok {
		t.Fatal("empty id resolved")
	}
}

func TestToolUseIndexCorruptEntryIsAMiss(t *testing.T) {
	spool := t.TempDir()
	if err := PutToolUse(spool, "call_1", "s"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(spool, stashDirName, toolUseDirName)
	entries, _ := os.ReadDir(dir)
	if err := os.WriteFile(filepath.Join(dir, entries[0].Name()), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := LookupToolUse(spool, "call_1"); ok {
		t.Fatal("a corrupt entry resolved")
	}
}

// A gated PreToolUse records its call under the session before the verdict, so
// the git hook of a commit made in that call's shell can find the session.
func TestPreToolUseWritesTheToolUseIndex(t *testing.T) {
	setHookEnv(t)
	pointSessionLogs(t)
	runHook(t, "PreToolUse", fixture(t, "pre-tool-use-bash", "s-index")) // tool_use_id call_0001; no core, denied
	got, ok := LookupToolUse(DefaultSpoolDir(), "call_0001")
	if !ok || got != "s-index" {
		t.Fatalf("LookupToolUse = %q, %v; want s-index", got, ok)
	}
}

// A subagent's call records the parent session its hooks re-key to.
func TestFoldedSubagentPreToolUseIndexesTheParentSession(t *testing.T) {
	root := pointSessionLogs(t)
	writeParentJournal(t, root, "sess-0001", linkRecord("sess-0001", "sess-0002", "skill-reminder"))
	setHookEnv(t)
	serveCore(t, fakecore.Script{Default: allowJSON})
	runHook(t, "SessionStart", fixture(t, "session-start-startup", ""))
	runHook(t, "SubagentStart", fixture(t, "subagent-start", ""))
	runHook(t, "PreToolUse", fixture(t, "pre-tool-use-bash", "sess-0002"))
	got, ok := LookupToolUse(DefaultSpoolDir(), "call_0001")
	if !ok || got != "sess-0001" {
		t.Fatalf("LookupToolUse = %q, %v; want the parent sess-0001", got, ok)
	}
}
