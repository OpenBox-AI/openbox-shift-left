package main

import (
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// TestRunEmitsProcStartAndExit is the trace's process-edge contract: every
// command dispatched through app.run, hook included, brackets its work with a
// proc.start / proc.exit pair carrying argv, version, os/arch and the exit
// code -- regardless of which command ran or what it returned.
func TestRunEmitsProcStartAndExit(t *testing.T) {
	dir := t.TempDir()
	w := &trace.Writer{Dir: dir, Now: func() time.Time { return time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC) }}
	restore := trace.SetDefault(w)
	defer restore()

	a, _, _ := testApp(nil)
	if code := a.run([]string{"help"}); code != exitOK {
		t.Fatalf("run(help) = %d, want %d", code, exitOK)
	}

	recs, skipped, err := trace.Read(dir, nil)
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	var sawStart, sawExit bool
	for _, r := range recs {
		if r.Proc != "help" {
			t.Errorf("record Proc = %q, want %q: %+v", r.Proc, "help", r)
		}
		switch r.Stage {
		case trace.StageProcStart:
			sawStart = true
			if r.Detail == nil {
				t.Fatalf("proc.start has no detail")
			}
		case trace.StageProcExit:
			sawExit = true
		}
	}
	if !sawStart || !sawExit {
		t.Fatalf("recs = %+v, want a proc.start and a proc.exit", recs)
	}
}

// TestRunEmitsProcExitEvenOnError: an unknown command still returns a proc.exit
// record with the real (non-zero) exit code, so a crashed/erroring run is
// still visible in the trace rather than looking like it never happened.
func TestRunEmitsProcExitEvenOnError(t *testing.T) {
	dir := t.TempDir()
	w := &trace.Writer{Dir: dir}
	restore := trace.SetDefault(w)
	defer restore()

	a, _, _ := testApp(nil)
	code := a.run([]string{"bogus-command"})
	if code == exitOK {
		t.Fatalf("run(bogus-command) = %d, want non-zero", code)
	}

	recs, _, err := trace.Read(dir, nil)
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	var exitCode any
	for _, r := range recs {
		if r.Stage == trace.StageProcExit {
			exitCode = r.Detail["exit_code"]
		}
	}
	// json round-trips through Read, so a float64.
	if exitCode == nil {
		t.Fatalf("no proc.exit record found: %+v", recs)
	}
	if got, ok := exitCode.(float64); !ok || int(got) != code {
		t.Fatalf("proc.exit exit_code = %v, want %d", exitCode, code)
	}
}

// TestHookDiagnosticsReachTheTrace: a hook's stderr is invisible when it
// exits 0, so a hook that sends nothing (here: an unknown provider) must say
// why in the trace, or the trace shows a run that did nothing for no reason.
func TestHookDiagnosticsReachTheTrace(t *testing.T) {
	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	defer restore()

	a, _, _ := testApp(nil)
	a.run([]string{"hook", "no-such-tool", "PreToolUse"})

	recs, _, err := trace.Read(dir, func(r trace.Record) bool { return r.Stage == trace.StageLog })
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	for _, r := range recs {
		if line, _ := r.Detail["line"].(string); strings.Contains(line, "unknown hook provider") {
			return
		}
	}
	t.Fatalf("no log record names the unknown provider; got %+v", recs)
}
