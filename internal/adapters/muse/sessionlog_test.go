package muse

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLog(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendLog(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func intentLine(seq int, tool, id, ts, input string) string {
	idField := ""
	if id != "" {
		idField = fmt.Sprintf(`"tool_use_id": %q, `, id)
	}
	return fmt.Sprintf(`{"seq": %d, "timestamp": %q, "type": "side_effect_intent", "session_id": "s", %s"tool_name": %q, "tool_input": %s}`+"\n",
		seq, ts, idField, tool, input)
}

const (
	t0 = "2026-09-30T00:00:01.000Z"
	t1 = "2026-09-30T00:00:02.000Z"
	t2 = "2026-09-30T00:00:03.000Z"
)

var cutoffAfterAll = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func read(t *testing.T, path string, cur fileCursor, cutoff time.Time) readOutcome {
	t.Helper()
	out, err := readLog(path, cur, cutoff, time.Now().Add(time.Minute), 1<<20)
	if err != nil {
		t.Fatalf("readLog: %v", err)
	}
	return out
}

func TestReadLogDecodesTheFixtureJoinFields(t *testing.T) {
	out := read(t, filepath.Join("testdata", "session-jsonl-sample.jsonl"), fileCursor{}, cutoffAfterAll)
	if out.DecodeErr != nil {
		t.Fatalf("decode error: %v", out.DecodeErr)
	}
	var names []string
	for _, it := range out.Intents {
		names = append(names, it.ToolName)
		if it.ToolUseID != "" {
			t.Errorf("the fixture carries no tool_use_id, got %q", it.ToolUseID)
		}
	}
	if strings.Join(names, ",") != "Bash,Write,mcp__docs__search" {
		t.Errorf("intents = %v", names)
	}
	if out.Lines != 5 {
		t.Errorf("lines = %d", out.Lines)
	}
}

func TestReadLogLeavesATruncatedLastLineForTheNextPass(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	second := intentLine(2, "Write", "tu-2", t1, `{"file_path":"/x"}`)
	writeLog(t, path, intentLine(1, "Bash", "tu-1", t0, `{}`)+second[:len(second)/2])

	first := read(t, path, fileCursor{}, cutoffAfterAll)
	if len(first.Intents) != 1 || first.DecodeErr != nil {
		t.Fatalf("first pass: %d intents, err %v", len(first.Intents), first.DecodeErr)
	}
	appendLog(t, path, second[len(second)/2:])
	next := read(t, path, first.Cursor, cutoffAfterAll)
	if len(next.Intents) != 1 || next.Intents[0].ToolUseID != "tu-2" {
		t.Fatalf("second pass intents = %+v", next.Intents)
	}
}

func TestReadLogCursorAdvanceIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeLog(t, path, intentLine(1, "Bash", "tu-1", t0, `{}`)+intentLine(2, "Bash", "tu-2", t1, `{}`))
	first := read(t, path, fileCursor{}, cutoffAfterAll)
	again := read(t, path, first.Cursor, cutoffAfterAll)
	if len(first.Intents) != 2 || len(again.Intents) != 0 || again.Cursor != first.Cursor {
		t.Fatalf("first %d intents, second %d, cursors %+v %+v", len(first.Intents), len(again.Intents), first.Cursor, again.Cursor)
	}
}

func TestReadLogRestartsOnAReplacedOrShortenedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	writeLog(t, path, intentLine(1, "Bash", "tu-1", t0, `{}`)+intentLine(2, "Bash", "tu-2", t1, `{}`))
	first := read(t, path, fileCursor{}, cutoffAfterAll)

	// Replaced: a new file of the same length, renamed over the old one.
	replacement := filepath.Join(dir, "new.jsonl")
	writeLog(t, replacement, intentLine(1, "Edit", "tu-9", t0, `{}`)+intentLine(2, "Edit", "tu-8", t1, `{}`))
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if first.Cursor.Inode != 0 {
		out := read(t, path, first.Cursor, cutoffAfterAll)
		if !out.Rotated || len(out.Intents) != 2 || out.Intents[0].ToolName != "Edit" {
			t.Errorf("a replaced file was not read afresh: rotated=%v intents=%+v", out.Rotated, out.Intents)
		}
	}

	// Shortened: the recorded offset is past the end.
	writeLog(t, path, intentLine(1, "Read", "tu-7", t0, `{}`))
	cur := first.Cursor
	cur.Offset = 1 << 20
	out := read(t, path, cur, cutoffAfterAll)
	if !out.Rotated || len(out.Intents) != 1 || out.Intents[0].ToolName != "Read" {
		t.Errorf("a shortened file was not read afresh: rotated=%v intents=%+v", out.Rotated, out.Intents)
	}
}

func TestReadLogStepsOverAnOversizeLineInConstantMemory(t *testing.T) {
	// The tool input is over a MiB and full of the characters a naive scanner
	// would stop at: quotes, braces, escapes and commas.
	nasty := strings.Repeat(`\"}],{\"x\":[\\`, 100_000)
	input := `{"file_path":"/x","content":"` + nasty + `","n":[1,2,{"a":"}"}]}`
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeLog(t, path, intentLine(1, "Write", "tu-big", t0, input)+intentLine(2, "Bash", "tu-2", t1, `{}`))

	out, err := readLog(path, fileCursor{}, cutoffAfterAll, time.Now().Add(time.Minute), 8<<20)
	if err != nil || out.DecodeErr != nil || len(out.Intents) != 2 {
		t.Fatalf("intents=%d err=%v", len(out.Intents), out.DecodeErr)
	}
	if out.Intents[0].ToolName != "Write" || out.Intents[0].ToolUseID != "tu-big" || out.Intents[1].ToolName != "Bash" {
		t.Errorf("intents = %+v", out.Intents)
	}
}

func TestReadLogIsBoundedOnAHugeLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	line := intentLine(1, "Bash", "", t0, `{"command":"`+strings.Repeat("x", 900)+`"}`)
	for written := 0; written < 50<<20; written += len(line) {
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	const bound = 1 << 20
	out, err := readLog(path, fileCursor{}, cutoffAfterAll, time.Now().Add(time.Minute), bound)
	if err != nil {
		t.Fatal(err)
	}
	if out.BytesRead < bound || out.BytesRead > bound+int64(len(line)) {
		t.Errorf("read %d bytes, want the %d bound plus at most one line", out.BytesRead, bound)
	}
	if want := int(out.BytesRead) / len(line); len(out.Intents) != want {
		t.Errorf("%d intents for %d bytes", len(out.Intents), out.BytesRead)
	}
	if out.Cursor.Offset != out.BytesRead {
		t.Errorf("cursor %d, consumed %d", out.Cursor.Offset, out.BytesRead)
	}
}

func TestReadLogStopsAtTheDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeLog(t, path, intentLine(1, "Bash", "tu-1", t0, `{}`))
	out, err := readLog(path, fileCursor{}, cutoffAfterAll, time.Now().Add(-time.Second), 1<<20)
	if err != nil || len(out.Intents) != 0 || out.Cursor.Offset != 0 {
		t.Fatalf("a pass past its deadline read %d intents, cursor %+v, err %v", len(out.Intents), out.Cursor, err)
	}
}

func TestReadLogLeavesIntentsNotOlderThanTheCutoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeLog(t, path, intentLine(1, "Bash", "tu-1", t0, `{}`)+intentLine(2, "Bash", "tu-2", t2, `{}`)+intentLine(3, "Bash", "tu-3", t1, `{}`))
	cutoff, _ := time.Parse(time.RFC3339Nano, t1)
	out := read(t, path, fileCursor{}, cutoff)
	if len(out.Intents) != 1 || out.Intents[0].ToolUseID != "tu-1" {
		t.Fatalf("intents = %+v", out.Intents)
	}
	if out.Cursor.Offset != int64(len(intentLine(1, "Bash", "tu-1", t0, `{}`))) {
		t.Errorf("the cursor moved past a line it did not reconcile: %d", out.Cursor.Offset)
	}
}

func TestReadLogStopsBeforeALineOutsideTheSchema(t *testing.T) {
	good := intentLine(1, "Bash", "tu-1", t0, `{}`)
	for name, bad := range map[string]string{
		"no type":          `{"seq": 2, "tool_name": "Bash"}` + "\n",
		"not json":         "garbage\n",
		"intent no name":   `{"type": "side_effect_intent", "timestamp": "` + t1 + `"}` + "\n",
		"intent bad stamp": `{"type": "side_effect_intent", "tool_name": "Bash", "timestamp": "soon"}` + "\n",
		"array":            "[1,2]\n",
	} {
		path := filepath.Join(t.TempDir(), "session.jsonl")
		writeLog(t, path, good+bad+intentLine(3, "Bash", "tu-3", t2, `{}`))
		out := read(t, path, fileCursor{}, cutoffAfterAll)
		if out.DecodeErr == nil {
			t.Errorf("%s: no decode error", name)
		}
		if len(out.Intents) != 1 || out.Cursor.Offset != int64(len(good)) {
			t.Errorf("%s: intents=%d cursor=%d, want the pass to stop before the line", name, len(out.Intents), out.Cursor.Offset)
		}
	}
}

func TestReadLogSkipsBlankLinesAndToleratesOtherRecordTypes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeLog(t, path, "\n"+`{"type":"model_call","messages":[{"role":"user","text":"hi"}]}`+"\n\n"+intentLine(1, "Bash", "tu-1", t0, `{}`))
	out := read(t, path, fileCursor{}, cutoffAfterAll)
	if out.DecodeErr != nil || len(out.Intents) != 1 {
		t.Fatalf("err=%v intents=%d", out.DecodeErr, len(out.Intents))
	}
}

func TestLocateSessionLogs(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	sess := filepath.Join(root, "2026", "09", "28", "sid-1")
	writeLog(t, filepath.Join(sess, sessionLogName), "")
	writeLog(t, filepath.Join(sess, "subagent", "sub-a", sessionLogName), "")
	writeLog(t, filepath.Join(sess, "subagent", "sub-b", sessionLogName), "")
	if err := os.MkdirAll(filepath.Join(sess, "subagent", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	logs, dir := locateSessionLogs(root, "sid-1", "", now)
	if dir != sess || len(logs) != 3 {
		t.Fatalf("dir=%s logs=%+v", dir, logs)
	}
	if logs[0].Key != "" || logs[1].Key != "subagent/sub-a" || logs[2].SubagentID != "sub-b" {
		t.Errorf("logs = %+v", logs)
	}

	if l, _ := locateSessionLogs(root, "sid-1", "", now.AddDate(0, 0, 30)); len(l) != 0 {
		t.Error("a session outside the bounded search was found")
	}
	if l, d := locateSessionLogs(root, "sid-1", sess, now.AddDate(0, 0, 30)); len(l) != 3 || d != sess {
		t.Error("the remembered session directory was not used")
	}
	if l, _ := locateSessionLogs(root, "no-such-session", "", now); len(l) != 0 {
		t.Error("an unknown session had logs")
	}
	for _, bad := range []string{"", "..", "../x", "a/b", `a\b`} {
		if l, _ := locateSessionLogs(root, bad, "", now); len(l) != 0 {
			t.Errorf("session id %q resolved", bad)
		}
	}
}

func TestReadLogStepsOverALineOverTheCapAndCountsIt(t *testing.T) {
	huge := strings.Repeat("x", maxLineBytes+1<<20)
	good := intentLine(3, "Bash", "tu-3", t2, `{}`)

	t.Run("fields before the cap still make an intent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "session.jsonl")
		line := `{"seq": 1, "timestamp": "` + t0 + `", "type": "side_effect_intent", "tool_name": "Write", "tool_use_id": "tu-huge", "tool_input": {"content": "` + huge + `"}}` + "\n"
		writeLog(t, path, line+good)
		out, err := readLog(path, fileCursor{}, cutoffAfterAll, time.Now().Add(time.Minute), 64<<20)
		if err != nil || out.DecodeErr != nil || out.Oversize != 1 || len(out.Intents) != 2 {
			t.Fatalf("oversize=%d intents=%d decodeErr=%v err=%v", out.Oversize, len(out.Intents), out.DecodeErr, err)
		}
		if out.Intents[0].ToolUseID != "tu-huge" || out.Intents[1].ToolUseID != "tu-3" {
			t.Errorf("intents = %+v", out.Intents)
		}
		if out.Cursor.Offset != int64(len(line)+len(good)) {
			t.Errorf("cursor %d not past both lines", out.Cursor.Offset)
		}
	})

	t.Run("fields only after the cap are skipped and the pass moves on", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "session.jsonl")
		line := `{"tool_input": {"content": "` + huge + `"}, "type": "side_effect_intent", "tool_name": "Write", "timestamp": "` + t0 + `"}` + "\n"
		writeLog(t, path, line+good)
		out, err := readLog(path, fileCursor{}, cutoffAfterAll, time.Now().Add(time.Minute), 64<<20)
		if err != nil || out.DecodeErr != nil || out.Oversize != 1 || len(out.Intents) != 1 || out.Intents[0].ToolUseID != "tu-3" {
			t.Fatalf("oversize=%d intents=%+v decodeErr=%v err=%v", out.Oversize, out.Intents, out.DecodeErr, err)
		}
	})
}

func TestLineScannerChecksTheDeadlineInsideALine(t *testing.T) {
	line := `{"type":"model_call","body":"` + strings.Repeat("y", 1<<20) + `"}` + "\n"
	sc := &lineScanner{r: bufioReader(line), deadline: time.Now().Add(-time.Second)}
	if _, st := sc.scan(); st != lineIncomplete {
		t.Fatalf("status = %v, want the line left uncommitted", st)
	}
	if sc.n >= int64(len(line)) {
		t.Errorf("scanned %d of %d bytes past the deadline", sc.n, len(line))
	}
}

func bufioReader(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }
