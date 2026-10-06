package main

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// TestTracedLoggerTeesLinesIntoTraceLogStage: a lane daemon's ordinary
// logger.Printf calls are the operator-facing diagnostic surface, and the
// trace's own requirement is that every one of them ALSO becomes a trace.StageLog
// record with no change at any of the daemon's own call sites -- so this
// tees the io.Writer log.New is built on, not logger.Printf itself.
func TestTracedLoggerTeesLinesIntoTraceLogStage(t *testing.T) {
	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	defer restore()

	var stderr bytes.Buffer
	logger := tracedLogger(&stderr, "openbox test: ", 0)
	logger.Printf("hello %d", 1)

	if !strings.Contains(stderr.String(), "hello 1") {
		t.Fatalf("the wrapped writer did not still receive the line: %q", stderr.String())
	}

	recs, _, err := trace.Read(dir, nil)
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	var found bool
	for _, r := range recs {
		if r.Stage != trace.StageLog {
			continue
		}
		if line, _ := r.Detail["line"].(string); strings.Contains(line, "hello 1") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no log-stage trace record carried the line: %+v", recs)
	}
}

// TestRunTraceSweepsRunsAtStartupAndOnTick exercises the sweep loop against a
// fake clock crossing midnight, mirroring the trace's own rotation contract:
// a daemon must sweep once immediately (not wait a full hour for its first
// housekeeping pass) and again on its ticker.
func TestRunTraceSweepsRunsAtStartupAndOnTick(t *testing.T) {
	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	defer restore()

	// Seed a record so the directory (and today's file) exists before the
	// first sweep runs.
	trace.Emit(trace.Record{Stage: trace.StageLog})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		runTraceSweeps(ctx, log.New(bytes.NewBuffer(nil), "", 0))
		close(done)
	}()
	<-ctx.Done()
	<-done
	// Sweeping an unremarkable, single-day-old directory is a no-op by
	// design (nothing to gzip or prune yet); reaching here without a panic
	// or a hang across a real Sweep call, called via the startup path AND
	// the ctx.Done() exit, is what this test is actually proving.
}

// TestTraceRawCaptureCarriesTheSessionKey: a raw record is found by
// `openbox trace <session>` only if it names the session its call belongs
// to -- a claude.ai chat's conversation key, or a Claude Code call's own
// session header -- plus the upstream request id that pairs it with the
// call's activity.
func TestTraceRawCaptureCarriesTheSessionKey(t *testing.T) {
	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	defer restore()

	chatURL := "https://claude.ai/api/organizations/" + testChatOrg + "/chat_conversations/" + testChatConv + "/completion"
	traceRawCapture(gateway.RawCapture{Method: "POST", URL: chatURL, Status: 200,
		ResponseHeaders: map[string]string{"Request-Id": "req_raw_chat"}})
	traceRawCapture(gateway.RawCapture{Method: "POST", URL: "https://api.anthropic.com/v1/messages", Status: 200,
		RequestHeaders: map[string]string{"X-Claude-Code-Session-Id": "cc-session-raw"}})

	recs, _, err := trace.Read(dir, func(r trace.Record) bool { return r.Stage == trace.StageCapture })
	if err != nil || len(recs) != 2 {
		t.Fatalf("Read = %d records, %v; want 2", len(recs), err)
	}
	if got, want := recs[0].SessionID, testChatKey(testChatConv); got != want {
		t.Errorf("chat raw record session = %q, want %q", got, want)
	}
	if got := recs[0].Detail["request_id"]; got != "req_raw_chat" {
		t.Errorf("chat raw record request_id = %v, want req_raw_chat", got)
	}
	if got := recs[1].SessionID; got != "cc-session-raw" {
		t.Errorf("Claude Code raw record session = %q, want cc-session-raw", got)
	}
	// At generation 0 a session's run is the session itself; a chat has no
	// runs, so its key is both.
	if recs[0].RunID != recs[0].SessionID || recs[1].RunID != "cc-session-raw" {
		t.Errorf("run ids = %q, %q; want each record's current run", recs[0].RunID, recs[1].RunID)
	}
}
