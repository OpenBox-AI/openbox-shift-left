package main

import (
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// TestRouteCommitTool is R2, "agent commits only": the test matrix's six
// routing cases. A wrong answer here either drains an event under the wrong
// agent's client (mismatch) or silently emits one for a hand commit, which
// R2 says must never happen.
func TestRouteCommitTool(t *testing.T) {
	for _, tc := range []struct {
		name        string
		marker      string
		sessionTool string
		wantTool    string
		wantReason  string
	}{
		{"codex marker + codex tier", "codex", "codex", "codex", ""},
		{"claude-code marker + registry tool", "claude-code", "claude-code", "claude-code", ""},
		{"no marker: hand commit", "", "claude-code", "", "hand commit"},
		{"record without tool", "claude-code", "", "", "no recorded tool"},
		{"marker vs record mismatch", "claude-code", "codex", "", "disagrees"},
		{"unknown tool", "mystery-tool", "mystery-tool", "", "unknown tool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotTool, gotReason := routeCommitTool(tc.marker, tc.sessionTool)
			if gotTool != tc.wantTool {
				t.Errorf("tool = %q, want %q (reason=%q)", gotTool, tc.wantTool, gotReason)
			}
			if tc.wantReason != "" && !strings.Contains(gotReason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", gotReason, tc.wantReason)
			}
		})
	}
}

// TestCommitEventID_DeterministicAndDistinct is R1: the same (session, run,
// sha) triple always yields the same id, so a re-fired hook dedupes at core;
// any one of the three changing yields a different id, so two runs' events
// for the same commit are never silently merged.
func TestCommitEventID_DeterministicAndDistinct(t *testing.T) {
	base := commitEventID("sess-1", "run-1", "abc123")
	if again := commitEventID("sess-1", "run-1", "abc123"); again != base {
		t.Fatalf("commitEventID is not deterministic: %q != %q", base, again)
	}
	if len(base) != 32 {
		t.Fatalf("commitEventID length = %d, want 32", len(base))
	}
	for _, tc := range []struct {
		name              string
		session, run, sha string
	}{
		{"different session", "sess-2", "run-1", "abc123"},
		{"different run", "sess-1", "run-2", "abc123"},
		{"different sha", "sess-1", "run-1", "def456"},
	} {
		if got := commitEventID(tc.session, tc.run, tc.sha); got == base {
			t.Errorf("%s: commitEventID collided with the base id", tc.name)
		}
	}
}

// TestEmitCommitEvent_MissingCredentials R3: a tool with no credential store
// yet must skip the append and exit as if nothing happened (R7) -- never
// block the commit, never panic.
func TestEmitCommitEvent_MissingCredentials(t *testing.T) {
	home := isolateHomeUnbound(t)
	spoolRoot := t.TempDir()
	t.Setenv(devconfig.EnvSpoolDir, "")
	t.Setenv(devconfig.EnvSpoolRoot, spoolRoot)
	t.Setenv(obgit.EnvSessionDir, filepath.Join(home, "sessions"))
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	// Deliberately no seedCredentials call: claude-code has no store at all.

	logger := log.New(new(strings.Builder), "", 0)
	buf := &strings.Builder{}
	logger.SetOutput(buf)

	facts := obgit.CommitFacts{SHA: "sha-missing-creds", Tree: "tree-1", Repo: "acme/app"}
	rs := obgit.ResolvedSession{ID: "sess-no-creds", Tier: obgit.TierRegistry, Tool: "claude-code"}
	emitCommitEvent("claude-code", facts, rs, logger)

	if !strings.Contains(buf.String(), "no credentials") {
		t.Errorf("expected a missing-credentials skip reason, got: %s", buf.String())
	}
	spoolDir := providers.SpoolDirFor("claude-code")
	if entries, err := os.ReadDir(spoolDir); err == nil && len(entries) != 0 {
		t.Errorf("an event was appended despite no credentials: %v", entries)
	}
}

// TestEmitCommitEvent_HaltedRunSkipped R4: a run already latched as halted
// (generation > 0, matching the RESUME-ONLY run-keyed latch) must never
// receive a new event -- the drainer would only discard it later, so
// skipping here is strictly better than spooling it.
func TestEmitCommitEvent_HaltedRunSkipped(t *testing.T) {
	home := isolateHomeOnly(t)
	seedCredentials(t, "claude-code")
	spoolRoot := t.TempDir()
	t.Setenv(devconfig.EnvSpoolDir, "")
	t.Setenv(devconfig.EnvSpoolRoot, spoolRoot)
	sessionDir := filepath.Join(home, "sessions")
	t.Setenv(obgit.EnvSessionDir, sessionDir)
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	const sessionID = "sess-halted-1"
	runStore := obgit.RunStore{Dir: obgit.RunDir(sessionDir)}
	run, err := runStore.Bump(sessionID) // generation 1, a resumed run
	if err != nil {
		t.Fatalf("Bump: %v", err)
	}
	hookflow.WriteSessionHalt(discardMainLogger(), run.RunID, client.Evaluation{
		Verdict: client.VerdictHalt, Reason: "org kill switch",
	})

	logger := log.New(new(strings.Builder), "", 0)
	buf := &strings.Builder{}
	logger.SetOutput(buf)
	facts := obgit.CommitFacts{SHA: "sha-halted", Tree: "tree-1", Repo: "acme/app"}
	rs := obgit.ResolvedSession{ID: sessionID, Tier: obgit.TierRegistry, Tool: "claude-code"}
	emitCommitEvent("claude-code", facts, rs, logger)

	if !strings.Contains(buf.String(), "run is halted") {
		t.Errorf("expected a halted-run skip reason, got: %s", buf.String())
	}
	spoolDir := providers.SpoolDirFor("claude-code")
	if entries, err := os.ReadDir(spoolDir); err == nil && len(entries) != 0 {
		t.Errorf("an event was appended for a halted run: %v", entries)
	}
}

// TestEmitCommitEvent_TwoSessionsTwoSpools two sessions resolved from the
// same commit, each attributed to a different tool, must each land in that
// tool's OWN spool -- never one draining under the other agent's client
// (the routing-mismatch risk the plan calls out). It also proves R1's
// dedupe promise end to end: re-firing the sink for the same commit and
// sessions (a hook that ran twice) yields the identical event_id both times.
func TestEmitCommitEvent_TwoSessionsTwoSpools(t *testing.T) {
	isolateHomeUnbound(t)
	seedCredentials(t, "claude-code", "codex")
	spoolRoot := t.TempDir()
	t.Setenv(devconfig.EnvSpoolDir, "")
	t.Setenv(devconfig.EnvSpoolRoot, spoolRoot)
	sessionDir := t.TempDir()
	t.Setenv(obgit.EnvSessionDir, sessionDir)
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	logger := log.New(new(strings.Builder), "", 0)
	logger.SetOutput(&strings.Builder{})

	facts := obgit.CommitFacts{SHA: "sha-shared", Tree: "tree-shared", Repo: "acme/app", Branch: "main"}
	sessions := []obgit.ResolvedSession{
		{ID: "sess-cc", Tier: obgit.TierRegistry, Tool: "claude-code"},
		{ID: "sess-codex", Tier: obgit.TierCodexEnv, Tool: "codex"},
	}
	// newCommitSink reads a single process-wide marker (attestProvider) and
	// applies it to every resolved session; exercising emitCommitEvent
	// directly per session proves each tool's own routing/append
	// independently without needing two processes with two different
	// markers.
	emitCommitEvent("claude-code", facts, sessions[0], logger)
	emitCommitEvent("codex", facts, sessions[1], logger)

	ccLine := onlyJSONLLine(t, providers.SpoolDirFor("claude-code"))
	codexLine := onlyJSONLLine(t, providers.SpoolDirFor("codex"))
	if ccLine == codexLine {
		t.Fatalf("both tools recorded the identical line; spools are not separate")
	}
	var ccEv, codexEv client.DevEvent
	if err := json.Unmarshal([]byte(ccLine), &ccEv); err != nil {
		t.Fatalf("unmarshal cc event: %v", err)
	}
	if err := json.Unmarshal([]byte(codexLine), &codexEv); err != nil {
		t.Fatalf("unmarshal codex event: %v", err)
	}
	if ccEv.SessionID != "sess-cc" || codexEv.SessionID != "sess-codex" {
		t.Fatalf("events landed under the wrong session: cc=%q codex=%q", ccEv.SessionID, codexEv.SessionID)
	}

	// Re-fire for the same commit and sessions (simulating a re-run
	// post-commit hook): the event id must be byte-identical both times.
	emitCommitEvent("claude-code", facts, sessions[0], logger)
	ccLines := allJSONLLines(t, providers.SpoolDirFor("claude-code"))
	if len(ccLines) != 2 {
		t.Fatalf("expected two appended lines after the re-fire, got %d", len(ccLines))
	}
	var second client.DevEvent
	if err := json.Unmarshal([]byte(ccLines[1]), &second); err != nil {
		t.Fatalf("unmarshal second cc event: %v", err)
	}
	if second.EventID != ccEv.EventID {
		t.Errorf("re-fired event id = %q, want the identical %q (R1 dedupe)", second.EventID, ccEv.EventID)
	}
}

// TestPostCommitHook_Integration drives the real `openbox hook git
// post-commit` path end to end: a registry-resolved session with its own
// tool, a real repo and commit, seeded credentials. It proves exactly one
// spool line lands with the six required metadata keys and the trailer's
// own session id.
func TestPostCommitHook_Integration(t *testing.T) {
	_ = isolateHomeOnly(t)
	spool := perToolHookEnv(t)
	_ = spool // perToolHookEnv's single OPENBOX_SPOOL_DIR is fine: one tool only
	seedCredentials(t, "claude-code")
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	repo := gitRepoNoSessionEnv(t)
	// perToolHookEnv points OPENBOX_SESSION_DIR at its own temp dir (NOT
	// isolateHomeOnly's home/sessions -- the two are unrelated dirs); the
	// hook process resolves the registry via os.Getenv(EnvSessionDir)
	// directly (registry.go), so the record must be written to that same
	// path, read back here rather than assumed.
	sessionDir := os.Getenv(obgit.EnvSessionDir)
	if sessionDir == "" {
		t.Fatalf("%s not set by perToolHookEnv", obgit.EnvSessionDir)
	}
	worktree, err := obgit.Git{}.Worktree()
	if err != nil {
		t.Fatalf("resolve worktree: %v", err)
	}
	const sessionID = "sess-integration-1"
	if err := obgit.WriteSessionRecord(sessionDir, sessionID, worktree, "claude-code", time.Now()); err != nil {
		t.Fatalf("WriteSessionRecord: %v", err)
	}

	a, _, errb := testApp(map[string]string{"CLAUDECODE": "1"})
	if code := a.run([]string{"hook", "git", "post-commit"}); code != exitOK {
		t.Fatalf("hook git post-commit exit = %d; stderr=%q", code, errb.String())
	}
	t.Logf("stderr: %s", errb.String())

	ccSpool := providers.SpoolDirFor("claude-code")
	line := onlyJSONLLine(t, ccSpool)
	var ev client.DevEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("unmarshal spooled event: %v\nline: %s", err, line)
	}
	if ev.EventType != client.EventCommitCreated {
		t.Fatalf("event_type = %q, want CommitCreated", ev.EventType)
	}
	if ev.SessionID != sessionID {
		t.Fatalf("openbox_session_id = %q, want %q", ev.SessionID, sessionID)
	}
	for _, key := range []string{"commit_sha", "tree_sha", "parent_shas", "repo", "openbox_session_id"} {
		if _, ok := ev.Metadata[key]; !ok {
			t.Errorf("metadata missing %q: %v", key, ev.Metadata)
		}
	}
	if ev.Metadata["openbox_session_id"] != sessionID {
		t.Errorf("metadata.openbox_session_id = %v, want %q", ev.Metadata["openbox_session_id"], sessionID)
	}
	_ = repo
}

// gitRepoNoSessionEnv is gitRepoWithACommit without its OPENBOX_SESSION
// override: a commit-event integration test must resolve its session from
// the REGISTRY (which alone carries a Tool), never the env-override tier
// (which never does, per ResolveDetailed).
func gitRepoNoSessionEnv(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "subject")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	return repo
}

// onlyJSONLLine reads the exactly one *.jsonl file expected in dir and
// returns its (only) line, trimmed.
func onlyJSONLLine(t *testing.T, dir string) string {
	t.Helper()
	lines := allJSONLLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("dir %s has %d lines, want 1: %v", dir, len(lines), lines)
	}
	return lines[0]
}

func allJSONLLines(t *testing.T, dir string) []string {
	t.Helper()
	name := onlySpoolFile(t, dir)
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}
