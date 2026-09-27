package main

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// writeTraceDay Emits recs through a Writer whose clock is pinned at ts, and
// returns the plain .jsonl path it wrote -- one file per call, since Emit
// always names the file after its own clock's date.
func writeTraceDay(t *testing.T, dir string, ts time.Time, recs []trace.Record) string {
	t.Helper()
	w := &trace.Writer{Dir: dir, Now: func() time.Time { return ts }}
	for _, r := range recs {
		w.Emit(r)
	}
	return filepath.Join(dir, "trace-"+ts.Format("2006-01-02")+".jsonl")
}

// gzipReplace compresses path into path+".gz" and removes path, mimicking
// what Sweep's own gzipFile does -- so a test can produce a mixed
// .jsonl/.jsonl.gz fixture set without waiting on a real Sweep (which would
// also prune by real wall-clock retention, a risk for fixtures dated in the
// past).
func gzipReplace(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := os.WriteFile(path+".gz", buf.Bytes(), 0o600); err != nil {
		t.Fatalf("writing %s.gz: %v", path, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing %s: %v", path, err)
	}
}

func TestTraceTimelineOrdersMixedPlainAndGzipFixtures(t *testing.T) {
	dir := t.TempDir()
	day1 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
	const sess = "sess-abc"

	p1 := writeTraceDay(t, dir, day1, []trace.Record{
		{SessionID: sess, RunID: "run-1", Stage: trace.StageProcStart, Outcome: "ok"},
		{SessionID: "other-session", RunID: "run-9", Stage: trace.StageProcStart, Outcome: "ok"},
	})
	gzipReplace(t, p1) // day1 is now trace-2026-09-20.jsonl.gz

	writeTraceDay(t, dir, day2, []trace.Record{
		{SessionID: sess, RunID: "run-1", Stage: trace.StageDeliverResult, Outcome: "accepted", EventID: "ev-1"},
	}) // day2 stays plain: trace-2026-09-21.jsonl

	a, out, errb := testApp(nil)
	code := a.runTrace([]string{sess, "--dir", dir})
	if code != exitOK {
		t.Fatalf("runTrace exit = %d, stderr = %s", code, errb.String())
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (session filter + gz+plain merge), got %d:\n%s", len(lines), out.String())
	}
	if !strings.HasPrefix(lines[0], "2026-09-20") {
		t.Fatalf("want day1 (gz) first, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], trace.StageProcStart) {
		t.Fatalf("line 0 missing stage: %s", lines[0])
	}
	if !strings.HasPrefix(lines[1], "2026-09-21") {
		t.Fatalf("want day2 (plain) second, got: %s", lines[1])
	}
	if !strings.Contains(lines[1], trace.StageDeliverResult) || !strings.Contains(lines[1], "event=ev-1") {
		t.Fatalf("line 1 missing expected fields: %s", lines[1])
	}
	if strings.Contains(out.String(), "other-session") {
		t.Fatalf("session filter leaked an unrelated session's record:\n%s", out.String())
	}
}

func TestTraceTimelineJSONElidesBodiesUnlessRequested(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 9, 22, 8, 0, 0, 0, time.UTC)
	writeTraceDay(t, dir, ts, []trace.Record{
		{SessionID: "sess-1", Stage: trace.StageHookIn, Detail: map[string]any{"raw": "super-secret-body"}},
	})

	a, out, _ := testApp(nil)
	if code := a.runTrace([]string{"sess-1", "--dir", dir, "--json"}); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(out.String(), "super-secret-body") {
		t.Fatalf("body leaked without --bodies: %s", out.String())
	}

	a2, out2, _ := testApp(nil)
	if code := a2.runTrace([]string{"sess-1", "--dir", dir, "--json", "--bodies"}); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out2.String(), "super-secret-body") {
		t.Fatalf("body missing with --bodies: %s", out2.String())
	}
}

func TestTraceListSummarizesSessions(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 9, 22, 8, 0, 0, 0, time.UTC)
	writeTraceDay(t, dir, ts, []trace.Record{
		{SessionID: "sess-a", Stage: trace.StageProcStart},
		{SessionID: "sess-a", Stage: trace.StageProcExit},
		{SessionID: "sess-b", Stage: trace.StageProcStart},
	})

	a, out, errb := testApp(nil)
	code := a.runTrace([]string{"--list", "--dir", dir})
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "sess-a") || !strings.Contains(got, "count=2") {
		t.Fatalf("missing sess-a summary: %s", got)
	}
	if !strings.Contains(got, "sess-b") || !strings.Contains(got, "count=1") {
		t.Fatalf("missing sess-b summary: %s", got)
	}
}

func TestTraceTimelineMissingDirIsNotAnError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	a, _, errb := testApp(nil)
	code := a.runTrace([]string{"whatever", "--dir", dir})
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
}

func TestTraceRequiresTargetArgument(t *testing.T) {
	a, _, _ := testApp(nil)
	code := a.runTrace([]string{"--dir", t.TempDir()})
	if code != exitError {
		t.Fatalf("exit = %d, want exitError", code)
	}
}

func TestTraceAgainstCoreRequiresAgentFlag(t *testing.T) {
	a, _, _ := testApp(nil)
	code := a.runTrace([]string{"sess-1", "--against-core", "--dir", t.TempDir()})
	if code != exitError {
		t.Fatalf("exit = %d, want exitError", code)
	}
}
