package trace

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeJSONL(t *testing.T, path string, lines ...string) {
	t.Helper()
	var buf bytes.Buffer
	for _, l := range lines {
		buf.WriteString(l)
		buf.WriteString("\n")
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeGZ(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	for _, l := range lines {
		gz.Write([]byte(l))
		gz.Write([]byte("\n"))
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip %s: %v", path, err)
	}
}

func rec(t *testing.T, ts time.Time, sessionID, stage string) string {
	t.Helper()
	r := Record{TS: ts, PID: 1, Proc: "openbox", SessionID: sessionID, Stage: stage}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	return string(b)
}

func TestReadSpansJSONLAndGZWithFilterAndOrder(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 9, 25, 11, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 9, 26, 9, 30, 0, 0, time.UTC)

	// Older day, gzipped: two records, one matching, one not.
	writeGZ(t, filepath.Join(dir, "trace-2026-09-25.jsonl.gz"),
		rec(t, t1, "sess-a", StageHookIn),
		rec(t, t0, "sess-b", StageHookOut),
	)
	// Today, plain: one matching record.
	writeJSONL(t, filepath.Join(dir, "trace-2026-09-26.jsonl"),
		rec(t, t3, "sess-a", StageDeliverResult),
		rec(t, t2, "sess-a", StageDeliverAttempt),
	)

	match := func(r Record) bool { return r.SessionID == "sess-a" }
	recs, skipped, err := Read(dir, match)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	if len(recs) != 3 {
		t.Fatalf("recs = %d, want 3: %+v", len(recs), recs)
	}
	// Sorted by TS ascending regardless of source file.
	wantOrder := []string{StageHookIn, StageDeliverAttempt, StageDeliverResult}
	for i, w := range wantOrder {
		if recs[i].Stage != w {
			t.Fatalf("recs[%d].Stage = %s, want %s (full: %+v)", i, recs[i].Stage, w, recs)
		}
	}
}

func TestReadCountsCorruptLinesAsSkipped(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)
	good := rec(t, ts, "sess-a", StageAuth)
	writeJSONL(t, filepath.Join(dir, "trace-2026-09-26.jsonl"),
		good,
		"{not valid json",
		"",
	)

	recs, skipped, err := Read(dir, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}
	if len(recs) != 1 || recs[0].Stage != StageAuth {
		t.Fatalf("recs = %+v, want one good record", recs)
	}
}

func TestReadNoMatchFunctionKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)
	writeJSONL(t, filepath.Join(dir, "trace-2026-09-26.jsonl"),
		rec(t, ts, "sess-a", StageAuth),
		rec(t, ts.Add(time.Second), "sess-b", StageDoctor),
	)

	recs, _, err := Read(dir, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("recs = %d, want 2", len(recs))
	}
}
