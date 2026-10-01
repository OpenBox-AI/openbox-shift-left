package muse

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/conformance"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

func TestMapTurnShape(t *testing.T) {
	stop := parseFixture(t, "stop")
	m := testMapper()
	m.CaptureContent = true
	m.RedactContent = strings.ToUpper
	started, completed, ok := m.MapTurn(stop, "think one", 3)
	if !ok || started.EventType != client.EventTurnStarted || completed.EventType != client.EventTurnCompleted {
		t.Fatalf("pair = %v %v %v", started.EventType, completed.EventType, ok)
	}
	for _, ev := range []client.DevEvent{started, completed} {
		if ev.TurnIndex == nil || *ev.TurnIndex != 3 || ev.Tokens != nil || ev.Cost != nil || ev.AgentID != "" {
			t.Errorf("%s: index/tokens/agent = %v %v %v", ev.EventType, ev.TurnIndex, ev.Tokens, ev.AgentID)
		}
		if ev.Metadata["turn_index"] != 3 || ev.Metadata["turn_id"] != "turn-0001" {
			t.Errorf("%s: metadata = %v", ev.EventType, ev.Metadata)
		}
		raw, _ := json.Marshal(ev)
		if err := conformance.ValidateDevEvent(raw, true); err != nil {
			t.Errorf("%s not conformant: %v\n%s", ev.EventType, err, raw)
		}
	}
	if started.Content != nil {
		t.Error("the started half carries content")
	}
	if completed.Content == nil || completed.Content.Output != "THE WORKSPACE IS EMPTY." || completed.Content.Thinking != "THINK ONE" {
		t.Errorf("content = %+v", completed.Content)
	}
	real := m
	real.NewID = nil
	s2, c2, _ := real.MapTurn(stop, "think one", 3)
	if s2.EventID == c2.EventID || s2.EventID == "" {
		t.Error("the two halves share an event id")
	}
}

func TestMapTurnCaptureOffCarriesNoContentAndEmptyTurnsAreSkipped(t *testing.T) {
	stop := parseFixture(t, "stop")
	_, completed, ok := testMapper().MapTurn(stop, "secret thought", 0)
	if !ok || completed.Content != nil {
		t.Fatalf("capture off: %+v %v", completed.Content, ok)
	}
	raw, _ := json.Marshal(completed)
	if err := conformance.ValidateDevEvent(raw, false); err != nil {
		t.Errorf("not conformant with capture off: %v", err)
	}
	empty := parseFixture(t, "stop")
	empty.LastAssistantMessage = ""
	if _, _, ok := testMapper().MapTurn(empty, "", 0); ok {
		t.Error("a turn with neither reply nor thinking was built")
	}
	m := testMapper()
	m.CaptureContent = true
	if _, c, ok := m.MapTurn(empty, "only thinking", 0); !ok || c.Content == nil || c.Content.Output != "" || c.Content.Thinking != "only thinking" {
		t.Errorf("thinking-only turn = %+v %v", c.Content, ok)
	}
}

func TestMapTurnTagsAFoldedSubagent(t *testing.T) {
	ev := parseFixture(t, "stop")
	ev.SubagentSessionID, ev.SubagentID = "sess-0002", "skill-reminder"
	started, completed, ok := testMapper().MapTurn(ev, "", 0)
	if !ok {
		t.Fatal("no pair")
	}
	for _, e := range []client.DevEvent{started, completed} {
		if e.AgentID != "skill-reminder" || e.Metadata["agent_type"] != subagentAgentType {
			t.Errorf("%s: %q %v", e.EventType, e.AgentID, e.Metadata)
		}
	}
}

const turnSession = "s-turn"

// openTurnSession runs the hooks that precede a Stop and points the journal
// root at a temp dir holding content for resp_a/resp_b under turnSession.
func openTurnSession(t *testing.T, journal string) {
	t.Helper()
	root := pointSessionLogs(t)
	raw, err := os.ReadFile(filepath.Join("testdata", journal))
	if err != nil {
		t.Fatal(err)
	}
	p := todaysLog(root, turnSession)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	runHook(t, "UserPromptSubmit", fixture(t, "user-prompt-submit", turnSession))
}

func postLLM(t *testing.T, responseID string) {
	t.Helper()
	payload := strings.ReplaceAll(fixture(t, "post-llm-call", turnSession), `"resp_0001"`, `"`+responseID+`"`)
	runHook(t, "PostLLMCall", payload)
}

func turnRows(f *fakecore.Server) []fakecore.Received {
	var out []fakecore.Received
	for _, r := range f.Inbox() {
		if r.ActivityType() == "llm_completion" && strings.Contains(r.ActivityID(), ":turn:") {
			out = append(out, r)
		}
	}
	return out
}

func completedOutput(t *testing.T, rows []fakecore.Received) map[string]any {
	t.Helper()
	for _, r := range rows {
		if r.EventType() == "ActivityCompleted" {
			out, _ := r.Body["activity_output"].(map[string]any)
			return out
		}
	}
	t.Fatal("no completed turn row")
	return nil
}

// Stop reports the turn: capture on gives the reply and the journal's
// reasoning summaries; each Stop is one started and one completed row.
func TestStopEgressesTheReplyAndThinkingUnderCapture(t *testing.T) {
	setHookEnv(t)
	t.Setenv(devconfig.EnvContentCapture, "1")
	f := serveCore(t, fakecore.Script{Default: allowJSON})
	openTurnSession(t, "session-jsonl-content.jsonl")
	postLLM(t, "resp_a")
	postLLM(t, "resp_b")
	runHook(t, "Stop", fixture(t, "stop", turnSession))
	flushSession(t, turnSession)

	rows := turnRows(f)
	if len(rows) != 2 {
		t.Fatalf("turn rows = %d, want a started and a completed", len(rows))
	}
	for _, r := range rows {
		if r.ActivityID() != turnSession+":turn:0" {
			t.Errorf("activity id = %q", r.ActivityID())
		}
	}
	out := completedOutput(t, rows)
	if out["reply_text"] != "The workspace is empty." {
		t.Errorf("reply_text = %v in %v", out["reply_text"], out)
	}
	if c, _ := out["content"].(string); !strings.Contains(c, "The workspace is empty.") {
		t.Errorf("content = %v", out["content"])
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), "Neutral summary one.") || !strings.Contains(string(raw), "Neutral summary two.") {
		t.Errorf("thinking summaries not on the row: %s", raw)
	}

	// The next turn takes the next index, and the first turn's ids are gone.
	runHook(t, "Stop", fixture(t, "stop", turnSession))
	flushSession(t, turnSession)
	if rows := turnRows(f); len(rows) != 4 || rows[2].ActivityID() != turnSession+":turn:1" {
		t.Errorf("second turn rows = %d", len(rows))
	}
}

// With capture off no reply text reaches the spool or the wire, and the pair
// still goes out.
func TestStopWithCaptureOffEmitsThePairWithoutAnyText(t *testing.T) {
	spool := setHookEnv(t)
	f := serveCore(t, fakecore.Script{Default: allowJSON})
	openTurnSession(t, "session-jsonl-content.jsonl")
	postLLM(t, "resp_b")
	runHook(t, "Stop", fixture(t, "stop", turnSession))
	flushSession(t, turnSession)

	if rows := turnRows(f); len(rows) != 2 {
		t.Fatalf("turn rows = %d, want the pair", len(rows))
	}
	for _, r := range f.Inbox() {
		raw, _ := json.Marshal(r.Body)
		for _, leak := range []string{"The workspace is empty", "Neutral summary", "reply_text"} {
			if strings.Contains(string(raw), leak) {
				t.Errorf("%s row carries %q: %s", r.EventType(), leak, raw)
			}
		}
	}
	_ = filepath.Walk(spool, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			if strings.Contains(string(b), "The workspace is empty") || strings.Contains(string(b), "Neutral final reply") {
				t.Errorf("%s holds reply text with capture off", p)
			}
		}
		return nil
	})
}

// A journal in a format the reader does not know costs the thinking only: the
// reply, which the hook payload carries, still goes out, and the trace says why.
func TestStopOverAnUnverifiedJournalEmitsOutputOnlyAndTraces(t *testing.T) {
	setHookEnv(t)
	t.Setenv(devconfig.EnvContentCapture, "1")
	f := serveCore(t, fakecore.Script{Default: allowJSON})
	openTurnSession(t, "session-jsonl-content-drift.jsonl")
	postLLM(t, "resp_a")
	runHook(t, "Stop", fixture(t, "stop", turnSession))
	flushSession(t, turnSession)

	out := completedOutput(t, turnRows(f))
	if out["reply_text"] != "The workspace is empty." {
		t.Errorf("output = %v", out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "summary") {
		t.Errorf("thinking egressed from an unverified journal: %s", raw)
	}
	var found bool
	for _, r := range traceRecords(t, trace.StageCapture) {
		if r.Outcome == "muse.content" {
			found = true
			if r.Detail["reason"] != string(ContentUnverified) {
				t.Errorf("reason = %v", r.Detail["reason"])
			}
		}
	}
	if !found {
		t.Error("no muse.content finding traced")
	}
}

// Stop never opens a run: with no record of the session nothing is reported.
func TestStopForAnUnknownSessionReportsNothing(t *testing.T) {
	spool := setHookEnv(t)
	t.Setenv(devconfig.EnvContentCapture, "1")
	runHook(t, "Stop", fixture(t, "stop", "s-unknown"))
	if n := (hookflow.Spool{Dir: spool}).PendingCount("s-unknown"); n != 0 {
		t.Errorf("spooled %d events for a session no run was opened for", n)
	}
}

func TestStopWithNoReplyAndNoThinkingReportsNoTurn(t *testing.T) {
	setHookEnv(t)
	f := serveCore(t, fakecore.Script{Default: allowJSON})
	runHook(t, "UserPromptSubmit", fixture(t, "user-prompt-submit", turnSession))
	payload := strings.Replace(fixture(t, "stop", turnSession), `"The workspace is empty."`, `null`, 1)
	runHook(t, "Stop", payload)
	flushSession(t, turnSession)
	if rows := turnRows(f); len(rows) != 0 {
		t.Errorf("turn rows = %d", len(rows))
	}
}

func TestPostLLMCallIndexesTheTurnAndStashesTheRequestOnlyUnderCapture(t *testing.T) {
	for _, capture := range []bool{false, true} {
		spool := setHookEnv(t)
		if capture {
			t.Setenv(devconfig.EnvContentCapture, "1")
		}
		runHook(t, "UserPromptSubmit", fixture(t, "user-prompt-submit", turnSession))
		postLLM(t, "resp_a")
		runHook(t, "PostLLMCall", strings.ReplaceAll(fixture(t, "post-llm-call", turnSession), `"resp_0001"`, `"`+EchoResponseID+`"`))

		_, stashed := TakeRequest(spool, "resp_a")
		if stashed != capture {
			t.Errorf("capture=%v: request stashed = %v", capture, stashed)
		}
		if _, echoed := TakeRequest(spool, EchoResponseID); echoed {
			t.Error("the echo id was stashed")
		}
		ids := TakeTurnResponses(spool, turnSession, "turn-0001")
		if len(ids) != 1 || ids[0] != "resp_a" {
			t.Errorf("capture=%v: turn index = %v, want [resp_a]: ids are recorded regardless of capture", capture, ids)
		}
	}
}

func TestSessionEndDrainsTheTurnIndex(t *testing.T) {
	spool := setHookEnv(t)
	serveCore(t, fakecore.Script{Default: allowJSON})
	runHook(t, "UserPromptSubmit", fixture(t, "user-prompt-submit", turnSession))
	postLLM(t, "resp_a")
	if _, err := os.Stat(turnIndexDir(spool, turnSession)); err != nil {
		t.Fatalf("no turn index before the end: %v", err)
	}
	endSession(t, fixture(t, "session-end", turnSession), turnSession)
	if _, err := os.Stat(turnIndexDir(spool, turnSession)); !os.IsNotExist(err) {
		t.Errorf("turn index survived SessionEnd: %v", err)
	}
}

// A folded subagent's turn is tagged and keeps a cursor of its own.
func TestFoldedSubagentStopReportsATaggedTurn(t *testing.T) {
	setHookEnv(t)
	root := pointSessionLogs(t)
	writeParentJournal(t, root, "sess-0001", linkRecord("sess-0001", "sess-0002", "skill-reminder"))
	f := serveCore(t, fakecore.Script{Default: allowJSON})
	runHook(t, "SessionStart", fixture(t, "session-start-startup", ""))
	runHook(t, "SubagentStart", fixture(t, "subagent-start", ""))
	payload := strings.Replace(fixture(t, "subagent-stop", ""), `"last_assistant_message": null`, `"last_assistant_message": "Child reply."`, 1)
	runHook(t, "SubagentStop", payload)
	flushSession(t, "sess-0001")
	rows := turnRows(f)
	if len(rows) != 2 {
		t.Fatalf("turn rows = %d", len(rows))
	}
	if id := rows[0].ActivityID(); id != "sess-0001:agent:skill-reminder:turn:0" {
		t.Errorf("activity id = %q", id)
	}
	if m := rowMeta(rows[0]); m["agent_type"] != subagentAgentType {
		t.Errorf("metadata = %v", m)
	}
}

func TestReadTurnThinkingGivesUpAtTheBudget(t *testing.T) {
	root := t.TempDir()
	start := time.Now()
	thinking, state := readTurnThinking(root, "s-none", []string{"resp_a"}, 120*time.Millisecond)
	if thinking != "" || state != ContentAbsent {
		t.Errorf("= %q %v", thinking, state)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("waited %v", d)
	}
}

// The last call's telemetry record arrives after SessionEnd, so its request
// body must still be there; only the TTL retires it.
func TestRequestStashSurvivesSessionEndAndExpiresByTTL(t *testing.T) {
	spool := setHookEnv(t)
	t.Setenv(devconfig.EnvContentCapture, "1")
	serveCore(t, fakecore.Script{Default: allowJSON})
	runHook(t, "UserPromptSubmit", fixture(t, "user-prompt-submit", turnSession))
	postLLM(t, "resp_a")
	postLLM(t, "resp_b")
	endSession(t, fixture(t, "session-end", turnSession), turnSession)
	if _, err := os.Stat(turnIndexDir(spool, turnSession)); !os.IsNotExist(err) {
		t.Errorf("turn index survived SessionEnd: %v", err)
	}
	if _, ok := TakeRequest(spool, "resp_a"); !ok {
		t.Fatal("request stash was cleared at SessionEnd")
	}
	now := fakeStashClock(t, time.Now())
	*now = now.Add(requestTTL + time.Minute)
	if _, ok := TakeRequest(spool, "resp_b"); ok {
		t.Error("request stash outlived its TTL")
	}
}
