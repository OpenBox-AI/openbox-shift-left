package muse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

func pointSessionLogs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	prev := sessionLogRoot
	sessionLogRoot = func() string { return root }
	t.Cleanup(func() { sessionLogRoot = prev })
	return root
}

func traceRecords(t *testing.T, stage string) []trace.Record {
	t.Helper()
	recs, _, err := trace.Read(trace.Dir(), func(r trace.Record) bool { return r.Stage == stage })
	if err != nil {
		t.Fatalf("trace read: %v", err)
	}
	return recs
}

func todaysLog(root, sid string) string {
	d := time.Now()
	return filepath.Join(root, d.Format("2006"), d.Format("01"), d.Format("02"), sid, sessionLogName)
}

// A gated PreToolUse lands on the gate ledger, so the journal's record of that
// call joins; a call the hook never saw does not, and Stop reports it once.
func TestStopReconcilesTheSessionJournalAgainstGatedCalls(t *testing.T) {
	setHookEnv(t)
	root := pointSessionLogs(t)
	const sid = "s-gap"

	runHook(t, "PreToolUse", fixture(t, "pre-tool-use-bash", sid)) // tool_use_id call_0001; no core, denied
	log := intentLine(1, "bash", "call_0001", t0, `{"command":"go test ./..."}`) +
		intentLine(2, "Write", "toolu-oversize", t1, `{"file_path":"/big"}`)
	writeLog(t, todaysLog(root, sid), log)

	stdout, stderr := runHook(t, "Stop", fixture(t, "stop", sid))
	if stdout != "" {
		t.Errorf("Stop wrote %q", stdout)
	}
	gaps := traceRecords(t, trace.StageEvidenceGap)
	if len(gaps) != 1 || gaps[0].Detail["tool_use_id"] != "toolu-oversize" || gaps[0].Detail["tool_name"] != "Write" {
		t.Fatalf("findings = %+v; stderr %q", gaps, stderr)
	}
	if strings.Contains(stderr, "go test") || strings.Contains(stderr, "/big") {
		t.Errorf("the hook logged journal content: %q", stderr)
	}

	runHook(t, "Stop", fixture(t, "stop", sid))
	runHook(t, "SessionEnd", fixture(t, "session-end", sid))
	if n := len(traceRecords(t, trace.StageEvidenceGap)); n != 1 {
		t.Errorf("%d findings after later passes, want the one", n)
	}
}

// The synthetic journal's call ids are the ones the PreToolUse hooks carry, so
// every call joins exactly; the one call with no hook is the single gap.
func TestStopJoinsTheSampleJournalOnCallID(t *testing.T) {
	setHookEnv(t)
	root := pointSessionLogs(t)
	const sid = "s-sample"
	sample, err := os.ReadFile(filepath.Join("testdata", "session-jsonl-sample.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	writeLog(t, todaysLog(root, sid), string(sample))

	runHook(t, "PreToolUse", fixture(t, "pre-tool-use-bash", sid))           // call_0001
	runHook(t, "PreToolUse", fixture(t, "pre-tool-use-bash-escalated", sid)) // call_0002
	runHook(t, "Stop", fixture(t, "stop", sid))

	gaps := traceRecords(t, trace.StageEvidenceGap)
	if len(gaps) != 1 || gaps[0].Detail["tool_use_id"] != "call_0003" || gaps[0].Detail["tool_name"] != "read_file" {
		t.Fatalf("findings = %+v", gaps)
	}
	for _, r := range traceRecords(t, trace.StageEvidenceReconcile) {
		if r.Outcome != "ok" {
			t.Errorf("reconcile outcome = %q", r.Outcome)
		}
	}
}

func TestStopOverAFullyGatedJournalFindsNoGaps(t *testing.T) {
	setHookEnv(t)
	root := pointSessionLogs(t)
	const sid = "s-clean"
	writeLog(t, todaysLog(root, sid), frameHeader+
		intentLine(1, "bash", "call_0001", t0, `{"command":"go test ./..."}`)+terminalLine(2, "bash", "call_0001", t1)+
		intentLine(3, "bash", "call_0002", t1, `{"command":"ls"}`)+terminalLine(4, "bash", "call_0002", t2))

	runHook(t, "PreToolUse", fixture(t, "pre-tool-use-bash", sid))
	runHook(t, "PreToolUse", fixture(t, "pre-tool-use-bash-escalated", sid))
	runHook(t, "Stop", fixture(t, "stop", sid))

	if n := len(traceRecords(t, trace.StageEvidenceGap)); n != 0 {
		t.Fatalf("%d findings over a journal whose every call was gated", n)
	}
	recs := traceRecords(t, trace.StageEvidenceReconcile)
	if len(recs) != 1 || recs[0].Outcome != "ok" || recs[0].Detail["intents"] != float64(2) || recs[0].Detail["gaps"] != float64(0) {
		t.Fatalf("reconcile records = %+v", recs)
	}
}

func TestSessionEndAlsoReconciles(t *testing.T) {
	setHookEnv(t)
	root := pointSessionLogs(t)
	const sid = "s-end"
	writeLog(t, todaysLog(root, sid), intentLine(1, "Write", "toolu-x", t0, `{}`))
	runHook(t, "SessionEnd", fixture(t, "session-end", sid))
	if n := len(traceRecords(t, trace.StageEvidenceGap)); n != 1 {
		t.Fatalf("%d findings", n)
	}
}

func TestStopWithNoJournalIsSilent(t *testing.T) {
	setHookEnv(t)
	pointSessionLogs(t)
	stdout, stderr := runHook(t, "Stop", fixture(t, "stop", "s-none"))
	if stdout != "" || stderr != "" {
		t.Errorf("stdout=%q stderr=%q", stdout, stderr)
	}
	if n := len(traceRecords(t, trace.StageEvidenceGap)) + len(traceRecords(t, trace.StageEvidenceReconcile)); n != 0 {
		t.Errorf("%d evidence records for a session with no journal", n)
	}
}

// The gate ledger is bookkeeping: a spool that cannot be written must not
// change what the gate answers.
func TestPreToolUseAnswerIsUnchangedWhenTheLedgerCannotBeWritten(t *testing.T) {
	setHookEnv(t)
	want, _ := runHook(t, "PreToolUse", fixture(t, "pre-tool-use-bash", "s-ledger"))
	if want == "" {
		t.Fatal("the reference run wrote no answer")
	}

	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(devconfig.EnvSpoolDir, filepath.Join(blocker, "spool"))
	got, stderr := runHook(t, "PreToolUse", fixture(t, "pre-tool-use-bash", "s-ledger"))
	if got != want {
		t.Errorf("answer changed with an unwritable spool:\n got %q\nwant %q", got, want)
	}
	if !strings.Contains(stderr, "gate ledger") {
		t.Errorf("the ledger failure was not logged: %q", stderr)
	}
}
