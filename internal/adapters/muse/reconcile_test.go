package muse

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// reconcileEnv is one scratch sessions root and spool, and the findings a pass
// emitted.
type reconcileEnv struct {
	t     *testing.T
	root  string
	spool string
	sid   string
	recs  []trace.Record
}

var reconcileNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func newReconcileEnv(t *testing.T) *reconcileEnv {
	t.Helper()
	return &reconcileEnv{t: t, root: t.TempDir(), spool: t.TempDir(), sid: "sid-1"}
}

func (e *reconcileEnv) logPath(sub string) string {
	p := filepath.Join(e.root, "2026", "09", "30", e.sid)
	if sub != "" {
		p = filepath.Join(p, "subagent", sub)
	}
	return filepath.Join(p, sessionLogName)
}

func (e *reconcileEnv) run(cutoff time.Time) Result {
	e.t.Helper()
	e.recs = nil
	return Reconciler{
		LogRoot: e.root, SpoolDir: e.spool,
		Now:  func() time.Time { return reconcileNow },
		Emit: func(r trace.Record) { e.recs = append(e.recs, r) },
	}.Run(e.sid, "run-1", cutoff, time.Now().Add(time.Minute))
}

func (e *reconcileEnv) gaps() []trace.Record {
	var out []trace.Record
	for _, r := range e.recs {
		if r.Stage == trace.StageEvidenceGap {
			out = append(out, r)
		}
	}
	return out
}

func (e *reconcileEnv) gate(tool, id string) {
	e.t.Helper()
	if err := RecordGateCall(e.spool, e.sid, tool, id); err != nil {
		e.t.Fatal(err)
	}
}

func TestReconcileExactJoinOnToolUseID(t *testing.T) {
	e := newReconcileEnv(t)
	writeLog(t, e.logPath(""),
		intentLine(1, "Bash", "tu-gated", t0, `{}`)+intentLine(2, "Write", "tu-ungated", t1, `{}`))
	e.gate("Bash", "tu-gated")
	// A gate record for another tool under the ungated id's name must not match:
	// the join is on the id, not on counts.
	e.gate("Write", "tu-other")

	res := e.run(cutoffAfterAll)
	if res.Intents != 2 || res.Gaps != 1 || res.Disabled {
		t.Fatalf("result = %+v", res)
	}
	g := e.gaps()
	if len(g) != 1 {
		t.Fatalf("%d findings", len(g))
	}
	d := g[0].Detail
	if g[0].Provider != "muse" || g[0].SessionID != "sid-1" || g[0].RunID != "run-1" ||
		d["provider"] != "muse" || d["session_id"] != "sid-1" || d["run_id"] != "run-1" ||
		d["tool_name"] != "Write" || d["tool_use_id"] != "tu-ungated" || d["reason"] != "no_gate_record" {
		t.Errorf("finding = %+v", g[0])
	}
}

func TestReconcileLooseJoinCountsByToolOrdinal(t *testing.T) {
	e := newReconcileEnv(t)
	writeLog(t, e.logPath(""),
		intentLine(1, "Write", "", t0, `{}`)+intentLine(2, "Write", "", t1, `{}`)+
			intentLine(3, "Bash", "", t1, `{}`)+intentLine(4, "Write", "", t2, `{}`))
	e.gate("Write", "")
	e.gate("Write", "")
	e.gate("Bash", "")

	res := e.run(cutoffAfterAll)
	if res.Intents != 4 || res.Gaps != 1 {
		t.Fatalf("result = %+v", res)
	}
	g := e.gaps()
	if len(g) != 1 || g[0].Detail["tool_name"] != "Write" {
		t.Fatalf("findings = %+v", g)
	}
	if _, has := g[0].Detail["tool_use_id"]; has {
		t.Error("a loose finding names a tool_use_id it does not have")
	}
}

func TestReconcileLooseOrdinalsSurviveAcrossPasses(t *testing.T) {
	e := newReconcileEnv(t)
	writeLog(t, e.logPath(""), intentLine(1, "Write", "", t0, `{}`))
	e.gate("Write", "")
	if res := e.run(cutoffAfterAll); res.Gaps != 0 {
		t.Fatalf("first pass: %+v", res)
	}
	// The second call was never gated: one intent, still one gate record.
	appendLog(t, e.logPath(""), intentLine(2, "Write", "", t1, `{}`))
	if res := e.run(cutoffAfterAll); res.Intents != 1 || res.Gaps != 1 {
		t.Fatalf("second pass: %+v", res)
	}
}

func TestReconcileSecondPassFindsNothingNew(t *testing.T) {
	e := newReconcileEnv(t)
	writeLog(t, e.logPath(""), intentLine(1, "Write", "tu-1", t0, `{}`))
	if res := e.run(cutoffAfterAll); res.Gaps != 1 {
		t.Fatalf("first pass: %+v", res)
	}
	res := e.run(cutoffAfterAll)
	if res.Intents != 0 || res.Gaps != 0 || len(e.gaps()) != 0 || len(e.recs) != 0 {
		t.Fatalf("second pass repeated itself: %+v, %d records", res, len(e.recs))
	}
}

func TestReconcileLeavesIntentsNewerThanTheTrigger(t *testing.T) {
	e := newReconcileEnv(t)
	writeLog(t, e.logPath(""), intentLine(1, "Bash", "tu-1", t0, `{}`)+intentLine(2, "Bash", "tu-2", t2, `{}`))
	e.gate("Bash", "tu-1")
	cutoff, _ := time.Parse(time.RFC3339Nano, t1)

	if res := e.run(cutoff); res.Intents != 1 || res.Gaps != 0 {
		t.Fatalf("pass before the newer intent: %+v", res)
	}
	// The hook for tu-2 may still have been running; once a later trigger has
	// passed it without a record, it is a gap.
	if res := e.run(cutoffAfterAll); res.Intents != 1 || res.Gaps != 1 {
		t.Fatalf("pass after: %+v", res)
	}
}

func TestReconcileRespectsItsDeadline(t *testing.T) {
	e := newReconcileEnv(t)
	writeLog(t, e.logPath(""), intentLine(1, "Write", "tu-1", t0, `{}`))
	res := Reconciler{LogRoot: e.root, SpoolDir: e.spool, Now: func() time.Time { return reconcileNow }}.
		Run(e.sid, "run-1", cutoffAfterAll, time.Now().Add(-time.Second))
	if res.Intents != 0 {
		t.Fatalf("a pass past its deadline reconciled %d intents", res.Intents)
	}
	if res := e.run(cutoffAfterAll); res.Gaps != 1 {
		t.Fatalf("the intent was lost by the late pass: %+v", res)
	}
}

func TestReconcileNoLogIsQuiet(t *testing.T) {
	e := newReconcileEnv(t)
	res := e.run(cutoffAfterAll)
	if !res.NoLog || res.Errors != 0 || res.Disabled || len(e.recs) != 0 {
		t.Fatalf("result = %+v, %d records", res, len(e.recs))
	}
	if entries, _ := os.ReadDir(e.spool); len(entries) != 0 {
		t.Errorf("a session with no log left state behind: %v", entries)
	}
}

func TestReconcileCoversSubagentLogs(t *testing.T) {
	e := newReconcileEnv(t)
	writeLog(t, e.logPath(""), intentLine(1, "Bash", "tu-main", t0, `{}`))
	writeLog(t, e.logPath("sub-a"), intentLine(1, "Write", "tu-sub-gated", t0, `{}`)+intentLine(2, "Write", "tu-sub-ungated", t1, `{}`))
	e.gate("Bash", "tu-main")
	// The subagent's hooks may carry its own session id.
	if err := RecordGateCall(e.spool, "sub-a", "Write", "tu-sub-gated"); err != nil {
		t.Fatal(err)
	}
	res := e.run(cutoffAfterAll)
	if res.Intents != 3 || res.Gaps != 1 {
		t.Fatalf("result = %+v", res)
	}
	if g := e.gaps(); len(g) != 1 || g[0].Detail["tool_use_id"] != "tu-sub-ungated" || g[0].SessionID != "sid-1" {
		t.Fatalf("findings = %+v", g)
	}
}

func TestReconcileDisablesItselfOnALineThatIsNotJSON(t *testing.T) {
	e := newReconcileEnv(t)
	writeLog(t, e.logPath(""),
		intentLine(1, "Write", "tu-1", t0, `{}`)+"this is not json\n"+intentLine(3, "Write", "tu-3", t1, `{}`))
	res := e.run(cutoffAfterAll)
	if !res.Disabled || res.Intents != 1 || res.Gaps != 1 {
		t.Fatalf("result = %+v", res)
	}
	last := e.recs[len(e.recs)-1]
	if last.Stage != trace.StageEvidenceReconcile || last.Outcome != "disabled" {
		t.Errorf("last record = %+v", last)
	}
	// Stuck at the line it cannot read: the next pass is disabled again and
	// reports nothing twice.
	res = e.run(cutoffAfterAll)
	if !res.Disabled || res.Gaps != 0 {
		t.Fatalf("second pass = %+v", res)
	}
}

// Kinds the reconciler has never heard of are the normal case in Muse's
// journal: they are skipped, the pass stays enabled, and the joins still work.
func TestReconcileSkipsUnknownKindsWithoutDisabling(t *testing.T) {
	e := newReconcileEnv(t)
	writeLog(t, e.logPath(""), frameHeader+
		envelopeLine(1, "runtime.user_intent.accepted", t0, `{"text":"hi"}`)+
		envelopeLine(2, "brand.new.type", t0, `{"kind":"never-seen"}`)+
		intentLine(3, "bash", "call_gated", t0, `{}`)+terminalLine(4, "bash", "call_gated", t1)+
		intentLine(5, "write_file", "call_ungated", t1, `{}`)+terminalLine(6, "write_file", "call_ungated", t2))
	e.gate("bash", "call_gated")
	res := e.run(cutoffAfterAll)
	if res.Disabled || res.Intents != 2 || res.Gaps != 1 {
		t.Fatalf("result = %+v", res)
	}
	if g := e.gaps(); len(g) != 1 || g[0].Detail["tool_use_id"] != "call_ungated" || g[0].Detail["tool_name"] != "write_file" {
		t.Fatalf("findings = %+v", g)
	}
}

func TestReconcileSwallowsAnUnreadableCursor(t *testing.T) {
	e := newReconcileEnv(t)
	writeLog(t, e.logPath(""), intentLine(1, "Write", "tu-1", t0, `{}`))
	dir := reconcileDir(e.spool)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateStem(e.sid)+".cursor"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := e.run(cutoffAfterAll)
	if res.Errors != 1 || res.Gaps != 1 || res.Disabled {
		t.Fatalf("result = %+v", res)
	}
}

// A finding is a count of a fact about a call, never the call: the tool input
// the journal holds must not reach any record.
func TestReconcileRecordsCarryNoContent(t *testing.T) {
	e := newReconcileEnv(t)
	secret := "MARKER-" + strings.Repeat("z", 12)
	writeLog(t, e.logPath(""), intentLine(1, "Write", "tu-1", t0, `{"file_path":"/x/`+secret+`","content":"`+secret+`"}`))
	e.run(cutoffAfterAll)
	if len(e.recs) == 0 {
		t.Fatal("nothing recorded")
	}
	raw, _ := json.Marshal(e.recs)
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "tool_input") {
		t.Errorf("a record carries journal content: %s", raw)
	}
}

func TestReconcileRejectsASessionIDThatWouldEscape(t *testing.T) {
	e := newReconcileEnv(t)
	e.sid = "../escape"
	if res := e.run(cutoffAfterAll); !res.NoLog {
		t.Fatalf("result = %+v", res)
	}
	if err := RecordGateCall(e.spool, "../escape", "Bash", "x"); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(e.spool); len(entries) != 0 {
		t.Errorf("an unsafe id wrote state: %v", entries)
	}
}

func TestGateLedgerRoundTrip(t *testing.T) {
	spool := t.TempDir()
	for _, g := range []gateEntry{{"Bash", "a"}, {"Bash", "b"}, {"Write", ""}} {
		if err := RecordGateCall(spool, "s", g.ToolName, g.ToolUseID); err != nil {
			t.Fatal(err)
		}
	}
	got := loadGates(spool, "s", "missing")
	if got.byTool["Bash"] != 2 || got.byTool["Write"] != 1 || !got.ids["a"] || !got.ids["b"] || len(got.ids) != 2 {
		t.Errorf("gates = %+v", got)
	}
}

func TestSweepStateRemovesOnlyOldFiles(t *testing.T) {
	spool := t.TempDir()
	dir := reconcileDir(spool)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old, fresh := filepath.Join(dir, "old.cursor"), filepath.Join(dir, "fresh.cursor")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * stateRetention)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	sweepState(spool, time.Now())
	if _, err := os.Stat(old); err == nil {
		t.Error("an old state file survived")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a fresh state file was removed")
	}
}

func TestSummarizeEvidence(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	emitAt := func(at time.Time, r trace.Record) {
		(&trace.Writer{Dir: dir, Proc: "t", Now: func() time.Time { return at }}).Emit(r)
	}
	gap := func(sid string) trace.Record {
		return trace.Record{Provider: "muse", SessionID: sid, Stage: trace.StageEvidenceGap, Outcome: "no_gate_record"}
	}
	rec := func(sid, outcome string) trace.Record {
		return trace.Record{Provider: "muse", SessionID: sid, Stage: trace.StageEvidenceReconcile, Outcome: outcome}
	}

	empty, err := SummarizeEvidence(filepath.Join(dir, "absent"), now)
	if err != nil || empty != (EvidenceSummary{}) {
		t.Fatalf("absent trace = %+v, %v", empty, err)
	}

	emitAt(now.Add(-10*24*time.Hour), gap("old"))
	emitAt(now.Add(-2*time.Hour), gap("a"))
	emitAt(now.Add(-time.Hour), gap("a"))
	emitAt(now.Add(-time.Hour), trace.Record{Provider: "codex", SessionID: "x", Stage: trace.StageEvidenceGap})
	emitAt(now.Add(-time.Hour), rec("a", "ok"))
	sum, err := SummarizeEvidence(dir, now)
	if err != nil || sum.Gaps != 2 || sum.Unverified {
		t.Fatalf("summary = %+v, %v", sum, err)
	}

	emitAt(now.Add(-30*time.Minute), rec("b", "disabled"))
	if sum, _ := SummarizeEvidence(dir, now); !sum.Unverified {
		t.Error("a disabled pass was not reported as unverified")
	}
	emitAt(now.Add(-10*time.Minute), rec("b", "ok"))
	if sum, _ := SummarizeEvidence(dir, now); sum.Unverified {
		t.Error("a later clean pass did not clear unverified")
	}
}

func TestReconcileJournalWithoutIdsJoinsHooksThatCarryThem(t *testing.T) {
	e := newReconcileEnv(t)
	writeLog(t, e.logPath(""), intentLine(1, "Bash", "", t0, `{}`)+intentLine(2, "Bash", "", t1, `{}`)+intentLine(3, "Bash", "", t2, `{}`))
	e.gate("Bash", "toolu-1")
	e.gate("Bash", "toolu-2")
	if res := e.run(cutoffAfterAll); res.Intents != 3 || res.Gaps != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestReconcileExactMatchesDoNotHideAnIdLessGap(t *testing.T) {
	e := newReconcileEnv(t)
	writeLog(t, e.logPath(""), intentLine(1, "Write", "tu-a", t0, `{}`)+intentLine(2, "Write", "", t1, `{}`))
	e.gate("Write", "tu-a")
	if res := e.run(cutoffAfterAll); res.Intents != 2 || res.Gaps != 1 {
		t.Fatalf("the gate record the exact join used was counted again: %+v", res)
	}
}

func TestReconcileFailedCursorSaveDoesNotDuplicate(t *testing.T) {
	e := newReconcileEnv(t)
	writeLog(t, e.logPath(""), intentLine(1, "Write", "tu-1", t0, `{}`))
	cursor := filepath.Join(reconcileDir(e.spool), stateStem(e.sid)+".cursor")
	if err := os.MkdirAll(cursor, 0o700); err != nil { // a directory where the file goes: no load, no save
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		res := e.run(cutoffAfterAll)
		if res.Errors == 0 || len(e.gaps()) != 0 {
			t.Fatalf("pass %d with an unkeepable cursor reported %d findings, %+v", i, len(e.gaps()), res)
		}
	}
	if err := os.RemoveAll(cursor); err != nil {
		t.Fatal(err)
	}
	if res := e.run(cutoffAfterAll); res.Gaps != 1 || len(e.gaps()) != 1 {
		t.Fatalf("once the cursor can be kept the intent reports exactly once: %+v", res)
	}
	if res := e.run(cutoffAfterAll); res.Intents != 0 || len(e.gaps()) != 0 {
		t.Fatalf("repeat: %+v", res)
	}
}

func TestReconcileSkipsAPassWhileAnotherHoldsTheSession(t *testing.T) {
	e := newReconcileEnv(t)
	writeLog(t, e.logPath(""), intentLine(1, "Write", "tu-1", t0, `{}`))
	if err := os.MkdirAll(reconcileDir(e.spool), 0o700); err != nil {
		t.Fatal(err)
	}
	held := flock.New(filepath.Join(reconcileDir(e.spool), stateStem(e.sid)+".lock"))
	if ok, err := held.TryLock(); !ok || err != nil {
		t.Fatalf("test lock: %v %v", ok, err)
	}
	res := e.run(cutoffAfterAll)
	if !res.Skipped || res.Intents != 0 || len(e.recs) != 0 {
		t.Fatalf("held pass = %+v, %d records", res, len(e.recs))
	}
	_ = held.Unlock()
	if res := e.run(cutoffAfterAll); res.Skipped || res.Gaps != 1 {
		t.Fatalf("after release = %+v", res)
	}
}

func TestSweepStateReachesOldFilesBehindFreshOnes(t *testing.T) {
	spool := t.TempDir()
	dir := reconcileDir(spool)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxSweep+50; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("a-fresh-%04d.gates", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := filepath.Join(dir, "z-old.gates")
	if err := os.WriteFile(old, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * stateRetention)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	sweepState(spool, time.Now())
	if _, err := os.Stat(old); err == nil {
		t.Error("an old file behind a long run of fresh ones was never reached")
	}
}

func TestRecordGateCallSurvivesAnUnwritableSpool(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecordGateCall(filepath.Join(blocker, "spool"), "s", "Bash", "a"); err == nil {
		t.Error("an unwritable spool reported success")
	}
}

func TestRecordGateCallConcurrentAppendsAreAllKept(t *testing.T) {
	spool := t.TempDir()
	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := RecordGateCall(spool, "s", "Bash", fmt.Sprintf("tu-%d", i)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if g := loadGates(spool, "s"); g.byTool["Bash"] != n || len(g.ids) != n {
		t.Fatalf("ledger holds %d entries, %d ids; want %d", g.byTool["Bash"], len(g.ids), n)
	}
}
