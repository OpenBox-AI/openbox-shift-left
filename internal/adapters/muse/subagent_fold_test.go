package muse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
)

// linkRecord is the envelope Muse 1.4.1 writes into the PARENT's journal when
// it spawns a reminder subagent, scrubbed to synthetic ids.
func linkRecord(parent, child, agent string) string {
	return `{"schema_version":1,"id":"rec-0001","stream":{"kind":"session","id":"` + parent + `"},"sequence":44,"record_type":"event","payload_type":"runtime.session","payload":{"kind":"run","run_id":"run-0001","event":{"kind":"memory_reminder_child_session_linked","parent_session_id":"` + parent + `","parent_run_id":"run-0001","reminder_agent_id":"` + agent + `","generation_id":1,"child_session_id":"` + child + `"}}}`
}

// writeParentJournal puts a journal for parent under today's date directory,
// holding the given lines.
func writeParentJournal(t *testing.T, root, parent string, lines ...string) {
	t.Helper()
	p := todaysLog(root, parent)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func rowMeta(r fakecore.Received) map[string]any {
	m, _ := r.Body["metadata"].(map[string]any)
	return m
}

// A subagent's events land in the session that spawned it, as a Claude Code
// subagent's do: one run, one WorkflowStarted, the subagent's start as a
// subagent_started signal, every one of its rows tagged with agent_id and
// agent_type, and its end closing nothing. The parent's run stays open.
func TestSubagentEventsFoldIntoTheParentSession(t *testing.T) {
	root := pointSessionLogs(t)
	writeParentJournal(t, root, "sess-0001", linkRecord("sess-0001", "sess-0002", "skill-reminder"))
	setHookEnv(t)
	f := serveCore(t, fakecore.Script{Default: allowJSON})

	runHook(t, "SessionStart", fixture(t, "session-start-startup", ""))
	// The child's first model call can reach a hook before its SubagentStart.
	runHook(t, "PreLLMCall", fixture(t, "pre-llm-call-subagent", ""))
	runHook(t, "SubagentStart", fixture(t, "subagent-start", ""))
	runHook(t, "PreToolUse", fixture(t, "pre-tool-use-bash", "sess-0002"))
	runHook(t, "SubagentStop", fixture(t, "subagent-stop", ""))
	runHook(t, "PreToolUse", fixture(t, "pre-tool-use-read", ""))
	flushSession(t, "sess-0001")

	if runs := startedFirst(t, f); len(runs) != 1 || runs[0] != "sess-0001" {
		t.Fatalf("runs started = %v, want the parent's run only", runs)
	}
	if n := rowCount(f, fakecore.WireWorkflowCompleted); n != 0 {
		t.Errorf("WorkflowCompleted rows = %d, want 0: a subagent ending must not end its parent", n)
	}
	var signal, gates, tools int
	for _, r := range f.Inbox() {
		if run, _ := r.Body["run_id"].(string); run != "sess-0001" {
			t.Errorf("a %s row is on run %q, want the parent's", r.EventType(), run)
		}
		m := rowMeta(r)
		if r.Body["signal_name"] == "subagent_started" {
			signal++
			if m["agent_id"] != "skill-reminder" || m["agent_type"] != subagentAgentType {
				t.Errorf("subagent_started metadata = %v", m)
			}
		}
		if m["agent_type"] != subagentAgentType {
			continue
		}
		if m["agent_id"] != "skill-reminder" {
			t.Errorf("a subagent row names agent_id %v, want skill-reminder (from the journal link)", m["agent_id"])
		}
		switch r.ActivityType() {
		case "model_call_gate":
			gates++
		case "bash":
			tools++
		}
	}
	if signal != 1 {
		t.Errorf("subagent_started signals = %d, want 1", signal)
	}
	if gates == 0 || tools == 0 {
		t.Errorf("tagged subagent rows: %d model_call_gate, %d bash; want both", gates, tools)
	}
}

// Folded into the parent's run, a subagent answers to the parent's halt latch:
// once the parent is halted, the subagent's gated calls are denied too.
func TestParentHaltDeniesItsSubagentsCalls(t *testing.T) {
	root := pointSessionLogs(t)
	writeParentJournal(t, root, "sess-0001", linkRecord("sess-0001", "sess-0002", "skill-reminder"))
	setHookEnv(t)
	serveCore(t, fakecore.Script{
		Default:  allowJSON,
		Verdicts: map[string]string{"call_halt": verdictJSON("halt", "policy violation")},
	})

	runHook(t, "SessionStart", fixture(t, "session-start-startup", ""))
	halted := strings.ReplaceAll(fixture(t, "pre-tool-use-bash", ""), "call_0001", "call_halt")
	runHook(t, "PreToolUse", halted)
	if len(haltLatches(t)) != 1 {
		t.Fatalf("the parent is not latched: %v", haltLatches(t))
	}
	out, _ := runHook(t, "PreLLMCall", fixture(t, "pre-llm-call-subagent", ""))
	if strings.TrimSpace(out) == "" {
		t.Fatal("a halted parent's subagent model call was allowed")
	}
}

// A subagent's prompt rides the parent's session, and a prompt_submitted
// signal's args are the goal: the folded prompt carries none, and says why.
func TestFoldedSubagentPromptIsNotTheGoal(t *testing.T) {
	root := pointSessionLogs(t)
	writeParentJournal(t, root, "sess-0001", linkRecord("sess-0001", "sess-0002", "skill-reminder"))
	setHookEnv(t)
	t.Setenv(devconfig.EnvContentCapture, "1")
	f := serveCore(t, fakecore.Script{Default: allowJSON})

	runHook(t, "SessionStart", fixture(t, "session-start-startup", ""))
	runHook(t, "UserPromptSubmit", fixture(t, "user-prompt-submit", "sess-0002"))
	flushSession(t, "sess-0001")

	var n int
	for _, r := range f.Inbox() {
		if r.Body["signal_name"] != "prompt_submitted" {
			continue
		}
		n++
		if args, ok := r.Body["signal_args"]; ok && args != nil {
			t.Errorf("a subagent prompt carries signal_args %v: it would become the parent's goal", args)
		}
		if m := rowMeta(r); m["prompt_source"] != subagentAgentType || m["agent_type"] != subagentAgentType {
			t.Errorf("folded prompt metadata = %v", m)
		}
	}
	if n != 1 {
		t.Fatalf("prompt_submitted rows = %d, want 1", n)
	}
}

// A session the lifecycle already knows is never scanned for a parent, so a
// main session pays for no journal scan after its first event, even when some
// journal would name it a child.
func TestKnownSessionIsNotFolded(t *testing.T) {
	root := t.TempDir()
	writeParentJournal(t, root, "sess-0001", linkRecord("sess-0001", "sess-0009", "skill-reminder"))
	l := lifecycle{Dir: t.TempDir()}
	l.save("sess-0009", RunIdentity{}, false)

	ev := &HookEvent{SessionID: "sess-0009"}
	l.foldSubagent(HookPreToolUse, ev, root)
	if ev.folded() || ev.SessionID != "sess-0009" {
		t.Fatalf("a known session was folded into %q", ev.SessionID)
	}
	if _, ok := l.loadLink("sess-0009"); ok {
		t.Error("a known session was scanned and a link recorded")
	}
}

// Once a child is recorded as having no parent, it stays unfolded, even if a
// link appears later: a child's events never split across two sessions.
func TestNegativeLinkKeepsAChildWhole(t *testing.T) {
	root := t.TempDir()
	l := lifecycle{Dir: t.TempDir()}
	ev := &HookEvent{SessionID: "sess-0002"}
	l.foldSubagent(HookSubagentStart, ev, root)
	if ev.folded() {
		t.Fatal("folded with no journal")
	}
	writeParentJournal(t, root, "sess-0001", linkRecord("sess-0001", "sess-0002", "skill-reminder"))
	ev = &HookEvent{SessionID: "sess-0002"}
	l.foldSubagent(HookPreLLMCall, ev, root)
	if ev.folded() {
		t.Error("a child recorded as unlinked was folded by a later hook")
	}
}

// Only a usable session id other than the child's own is accepted as a parent,
// and a parent that is itself a folded subagent resolves to the top session.
func TestLinkResolution(t *testing.T) {
	root := t.TempDir()
	writeParentJournal(t, root, "sess-bad",
		`not json but names sess-0002 and "parent_session_id"`,
		linkRecord("../escape", "sess-0002", "x"),
		linkRecord("sess-0002", "sess-0002", "x"))
	if p, _, ok := findParentLink(root, "sess-0002", nowFn()); ok {
		t.Fatalf("an unusable parent %q was accepted", p)
	}

	writeParentJournal(t, root, "sess-0003", linkRecord("sess-0003", "sess-0004", "verify-reminder"))
	l := lifecycle{Dir: t.TempDir()}
	l.saveLink(subagentLink{ChildSessionID: "sess-0003", ParentSessionID: "sess-0001"})
	link := l.resolveLink("sess-0004", root)
	if link.ParentSessionID != "sess-0001" || link.AgentID != "verify-reminder" {
		t.Errorf("link = %+v, want parent sess-0001 via sess-0003, agent verify-reminder", link)
	}
}
