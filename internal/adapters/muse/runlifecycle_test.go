package muse

import (
	"strings"
	"testing"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
)

// startedFirst fails for any run whose rows reach core before that run's own
// WorkflowStarted did, and returns the run ids in the order they started.
func startedFirst(t *testing.T, f *fakecore.Server) []string {
	t.Helper()
	var order []string
	started := map[string]bool{}
	for _, r := range f.Inbox() {
		run, _ := r.Body["run_id"].(string)
		switch r.EventType() {
		case fakecore.WireWorkflowStarted:
			if started[run] {
				t.Errorf("run %q started twice", run)
			}
			started[run] = true
			order = append(order, run)
		case fakecore.WireWorkflowCompleted:
		default:
			if !started[run] {
				t.Errorf("a %s row of run %q reached core before its WorkflowStarted", r.EventType(), run)
			}
		}
	}
	return order
}

// Muse fires no SessionStart on `muse resume`: it sends the session's events
// under the old id after SessionEnd. The first one opens a new run, announced
// by its own WorkflowStarted, and the new run is not latched.
func TestResumeWithoutSessionStartOpensAnUnlatchedRun(t *testing.T) {
	setHookEnv(t)
	const sess = "s-resumed"
	f := serveCore(t, fakecore.Script{
		Default:  allowJSON,
		Verdicts: map[string]string{"call_halt": verdictJSON("halt", "policy violation")},
	})

	runHook(t, "SessionStart", fixture(t, "session-start-startup", sess))
	halted := strings.ReplaceAll(fixture(t, "pre-tool-use-bash", sess), "call_0001", "call_halt")
	out, _ := runHook(t, "PreToolUse", halted)
	if decodeStdout(t, out)["hookSpecificOutput"].(map[string]any)["permissionDecision"] != "deny" {
		t.Fatalf("the HALT did not deny: %q", out)
	}
	if len(haltLatches(t)) != 1 {
		t.Fatalf("run 1 is not latched: %v", haltLatches(t))
	}
	endSession(t, fixture(t, "session-end", sess), sess)

	before := f.Hits()
	for _, tc := range []struct{ hook, file string }{
		{"UserPromptSubmit", "user-prompt-submit"},
		{"PreToolUse", "pre-tool-use-read"},
	} {
		out, _ := runHook(t, tc.hook, fixture(t, tc.file, sess))
		if strings.TrimSpace(out) != "" {
			if hso, ok := decodeStdout(t, out)["hookSpecificOutput"].(map[string]any); ok && hso["permissionDecision"] == "deny" {
				t.Errorf("%s of the resumed run is still denied: %q", tc.hook, out)
			}
		}
	}
	if f.Hits() == before {
		t.Error("the resumed run's gated calls never reached core: the latch still answered them")
	}
	runs := startedFirst(t, f)
	if len(runs) != 2 || runs[0] != sess || runs[1] == sess {
		t.Fatalf("runs started = %v, want the session's first run then a minted one", runs)
	}
	if n := rowCount(f, fakecore.WireWorkflowStarted); n != 2 {
		t.Errorf("WorkflowStarted rows = %d, want 2 (one per run)", n)
	}
}

// A second event of the resumed run does not open a run again.
func TestResumedRunOpensOnce(t *testing.T) {
	setHookEnv(t)
	const sess = "s-resumed-once"
	f := serveCore(t, fakecore.Script{Default: allowJSON})
	runHook(t, "SessionStart", fixture(t, "session-start-startup", sess))
	endSession(t, fixture(t, "session-end", sess), sess)
	for i := 0; i < 3; i++ {
		runHook(t, "UserPromptSubmit", fixture(t, "user-prompt-submit", sess))
	}
	if runs := startedFirst(t, f); len(runs) != 2 {
		t.Errorf("runs started = %v, want 2", runs)
	}
}

// A subagent no parent journal links is a session of its own. Its first event may
// reach a hook before its SubagentStart does: that event opens the run, and
// the SubagentStart that follows does not start it a second time.
func TestSubagentSessionIsOpenedOnceWhicheverEventComesFirst(t *testing.T) {
	setHookEnv(t)
	f := serveCore(t, fakecore.Script{Default: allowJSON})

	runHook(t, "PreLLMCall", fixture(t, "pre-llm-call-subagent", ""))
	runHook(t, "SubagentStart", fixture(t, "subagent-start", ""))
	runHook(t, "SubagentStop", fixture(t, "subagent-stop", ""))
	flushSession(t, "sess-0002")

	runs := startedFirst(t, f)
	if len(runs) != 1 || runs[0] != "sess-0002" {
		t.Fatalf("runs started = %v, want the subagent's own session once", runs)
	}
	if n := rowCount(f, fakecore.WireWorkflowCompleted); n != 1 {
		t.Errorf("WorkflowCompleted rows = %d, want 1", n)
	}
	if len(gateRows(f)) == 0 {
		t.Error("a subagent's model call was not gated")
	}
}

// SubagentStart first is itself the child's SessionStarted, and a gated call
// inside the subagent still reaches the server: nothing is skipped by
// subagent_id.
func TestSubagentStartOpensTheChildAndItsCallsAreStillGated(t *testing.T) {
	setHookEnv(t)
	f := serveCore(t, fakecore.Script{Default: allowJSON,
		VerdictsByActivityType: map[string]string{"model_call_gate": verdictJSON("halt", "no")}})

	runHook(t, "SubagentStart", fixture(t, "subagent-start", ""))
	out, _ := runHook(t, "PreLLMCall", fixture(t, "pre-llm-call-subagent", ""))
	if strings.TrimSpace(out) == "" {
		t.Fatal("a subagent's denied model call was allowed")
	}
	if runs := startedFirst(t, f); len(runs) != 1 {
		t.Errorf("runs started = %v, want 1", runs)
	}
}

// A session id no run is known for is opened before its first event, whatever
// that event is.
func TestUnknownSessionGetsASessionStartedFirst(t *testing.T) {
	setHookEnv(t)
	f := serveCore(t, fakecore.Script{Default: allowJSON})
	runHook(t, "PreToolUse", fixture(t, "pre-tool-use-read", "s-never-started"))
	runs := startedFirst(t, f)
	if len(runs) != 1 || runs[0] != "s-never-started" {
		t.Fatalf("runs started = %v", runs)
	}
}

// A second SessionEnd of an ended run reports nothing and does not reopen it.
func TestSecondEndOfAnEndedRunIsDropped(t *testing.T) {
	setHookEnv(t)
	f := serveCore(t, fakecore.Script{Default: allowJSON})
	runHook(t, "SessionStart", fixture(t, "session-start-startup", "s-twice"))
	endSession(t, fixture(t, "session-end", "s-twice"), "s-twice")
	endSession(t, fixture(t, "session-end", "s-twice"), "s-twice")
	if n := rowCount(f, fakecore.WireWorkflowCompleted); n != 1 {
		t.Errorf("WorkflowCompleted rows = %d, want 1", n)
	}
	if n := rowCount(f, fakecore.WireWorkflowStarted); n != 1 {
		t.Errorf("WorkflowStarted rows = %d, want 1", n)
	}
}

// Muse kills a SessionEnd hook, and a subagent's hooks, as soon as it is done
// with them, so none of those three may start a delivery of its own: a killed
// attempt leaves a half-sent file the next drain can only discard. Their events
// wait in the spool for the detached flusher.
func TestEndingAndSubagentHooksLeaveDeliveryToTheFlusher(t *testing.T) {
	spool := setHookEnv(t)
	f := serveCore(t, fakecore.Script{Default: allowJSON})

	runHook(t, "SessionStart", fixture(t, "session-start-startup", "s-late"))
	before := len(f.Inbox())
	runHook(t, "SessionEnd", fixture(t, "session-end", "s-late"))
	runHook(t, "SubagentStart", fixture(t, "subagent-start", ""))
	runHook(t, "SubagentStop", fixture(t, "subagent-stop", ""))
	if got := len(f.Inbox()); got != before {
		t.Fatalf("an ending or subagent hook delivered %d event(s) inline; it must leave them to the flusher", got-before)
	}
	sp := hookflow.Spool{Dir: spool}
	for _, sess := range []string{"s-late", "sess-0002"} {
		if sp.PendingCount(sess) == 0 {
			t.Errorf("%s: nothing waits in the spool for the flusher", sess)
		}
	}
	flushSession(t, "s-late")
	flushSession(t, "sess-0002")
	if n := rowCount(f, fakecore.WireWorkflowCompleted); n != 2 {
		t.Errorf("after the flusher ran, WorkflowCompleted rows = %d, want 2", n)
	}
}

// A straggler of a session SessionEnd already sealed (a PostToolUse racing
// teardown) opens no run and spools nothing: only a prompt resumes a session.
func TestStragglerAfterSessionEndDoesNotReopenTheRun(t *testing.T) {
	setHookEnv(t)
	const sess = "s-straggler"
	f := serveCore(t, fakecore.Script{Default: allowJSON})
	runHook(t, "SessionStart", fixture(t, "session-start-startup", sess))
	endSession(t, fixture(t, "session-end", sess), sess)

	runHook(t, "PostToolUse", fixture(t, "post-tool-use", sess))
	runHook(t, "PostLLMCall", fixture(t, "post-llm-call", sess))
	flushSession(t, sess)

	if n := rowCount(f, fakecore.WireWorkflowStarted); n != 1 {
		t.Errorf("WorkflowStarted rows = %d, want 1: a straggler reopened the run", n)
	}
	if runs := startedFirst(t, f); len(runs) != 1 || runs[0] != sess {
		t.Errorf("runs started = %v, want only the session's own", runs)
	}
	l := lifecycle{Dir: lifecycleDir(DefaultSpoolDir())}
	if st, ok := l.load(sess); !ok || !st.Sealed || st.RunID != sess {
		t.Errorf("state after stragglers = %+v, want the original run still sealed", st)
	}
}

// A gated straggler of a sealed session is still evaluated by core, under the
// sealed run, and opens no run.
func TestGatedStragglerIsEvaluatedUnderTheSealedRun(t *testing.T) {
	setHookEnv(t)
	const sess = "s-gated-straggler"
	f := serveCore(t, fakecore.Script{Default: allowJSON})
	runHook(t, "SessionStart", fixture(t, "session-start-startup", sess))
	endSession(t, fixture(t, "session-end", sess), sess)

	before := f.Hits()
	runHook(t, "PreToolUse", fixture(t, "pre-tool-use-read", sess))
	if f.Hits() == before {
		t.Error("a gated straggler never reached /evaluate")
	}
	if n := rowCount(f, fakecore.WireWorkflowStarted); n != 1 {
		t.Errorf("WorkflowStarted rows = %d, want 1", n)
	}
	l := lifecycle{Dir: lifecycleDir(DefaultSpoolDir())}
	if st, _ := l.load(sess); !st.Sealed || st.RunID != sess {
		t.Errorf("a gated straggler reopened the run: %+v", st)
	}
}

// A folded subagent's event never reopens a sealed parent, even a
// prompt-shaped one; the session's own prompt does.
func TestFoldedEventNeverReopensASealedParent(t *testing.T) {
	l := lifecycle{Dir: t.TempDir(), Runs: obgit.RunStore{Dir: t.TempDir()}}
	l.save("sess-p", RunIdentity{}, true)
	ev := &HookEvent{SessionID: "sess-p", SubagentSessionID: "sess-c"}
	if _, tr := l.advance(HookUserPromptSubmit, ev); tr.Open || tr.Resumed {
		t.Errorf("a folded event reopened the sealed parent: %+v", tr)
	}
	ev = &HookEvent{SessionID: "sess-p"}
	if _, tr := l.advance(HookUserPromptSubmit, ev); !tr.Open || !tr.Resumed {
		t.Errorf("the session's own prompt did not resume it: %+v", tr)
	}
}
