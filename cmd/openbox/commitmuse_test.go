package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/muse"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// museCommitEvents runs the real post-commit hook for a commit made inside a
// Muse tool call (toolUseID is MUSE_TOOL_USE_ID in the hook's environment, ""
// for a hand commit) and returns the commit_created events the Muse spool
// holds afterwards.
func museCommitEvents(t *testing.T, toolUseID string, seed func(spool string)) []client.DevEvent {
	t.Helper()
	if testing.Short() {
		t.Skip("drives a real git repo; skipped in -short")
	}
	home := isolateHomeUnbound(t)
	perToolHookEnv(t)
	t.Setenv("OPENBOX_HALT_DIR", filepath.Join(home, "halted-sessions"))
	seedCredentials(t, "muse")
	gitRepoWithACommit(t)
	// A Muse tool shell carries no OpenBox session variable.
	t.Setenv(obgit.EnvSession, "")
	t.Setenv(obgit.EnvSessionFile, "")

	spool := muse.DefaultSpoolDir()
	if seed != nil {
		seed(spool)
	}

	env := map[string]string{}
	if toolUseID != "" {
		env["MUSE_TOOL_USE_ID"] = toolUseID
		t.Setenv("MUSE_TOOL_USE_ID", toolUseID)
	}
	a, _, errb := testApp(env)
	if code := a.run([]string{"hook", "git", "post-commit"}); code != exitOK {
		t.Fatalf("post-commit exit = %d; stderr=%q", code, errb.String())
	}

	var out []client.DevEvent
	files, _ := filepath.Glob(filepath.Join(providers.SpoolDirFor("muse"), "*.jsonl"))
	sort.Strings(files)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var ev client.DevEvent
			if line != "" && json.Unmarshal([]byte(line), &ev) == nil && ev.EventType == client.EventCommitCreated {
				out = append(out, ev)
			}
		}
	}
	return out
}

func TestMuseCommitInAToolShellEmitsOneCommitCreatedInTheIndexedSession(t *testing.T) {
	evs := museCommitEvents(t, "call_commit_1", func(spool string) {
		if err := muse.PutToolUse(spool, "call_commit_1", "muse-parent-session"); err != nil {
			t.Fatal(err)
		}
	})
	if len(evs) != 1 {
		t.Fatalf("commit_created events = %d, want exactly 1", len(evs))
	}
	if evs[0].SessionID != "muse-parent-session" {
		t.Fatalf("session = %q, want the indexed (governed) session", evs[0].SessionID)
	}
}

func TestMuseHandCommitEmitsNoCommitCreated(t *testing.T) {
	evs := museCommitEvents(t, "", func(spool string) {
		if err := muse.PutToolUse(spool, "call_commit_1", "muse-parent-session"); err != nil {
			t.Fatal(err)
		}
	})
	if len(evs) != 0 {
		t.Fatalf("a hand commit produced %d commit_created events", len(evs))
	}
}

func TestMuseCommitWithAnUnindexedToolUseIDEmitsNoCommitCreated(t *testing.T) {
	if evs := museCommitEvents(t, "call_never_gated", nil); len(evs) != 0 {
		t.Fatalf("an id the index never saw produced %d commit_created events", len(evs))
	}
}
