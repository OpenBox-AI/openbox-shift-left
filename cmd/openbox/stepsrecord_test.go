package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// TestSetupLaneEmitsUnitAndActivationRecordsInOrder is phase 3's own gap: a
// lane install (setupTelemetry/setupTransport, through the shared setupLane)
// wrote no trace record at all before this -- only the unit's OPENBOX_TRACE_DIR
// env key (laneunitenv_test.go) and the election watcher (StageElection) were
// covered. Every step setupLane takes -- writing the unit, starting it,
// proving it listens, then activating it -- must show up in order, through
// the fake-seamed harness so no real daemon or sudo call is ever reached.
func TestSetupLaneEmitsUnitAndActivationRecordsInOrder(t *testing.T) {
	skipUnlessSupervised(t)
	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	defer restore()

	h := newLaneHarness(t)
	a, _, _ := testApp(map[string]string{"HOME": h.home})

	if _, err := a.setupTelemetry(h.home, "127.0.0.1:18790", false); err != nil {
		t.Fatalf("setupTelemetry: %v", err)
	}

	recs, skipped, err := trace.Read(dir, nil)
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}

	var stages []string
	for _, r := range recs {
		stages = append(stages, r.Stage)
	}
	var sawWrite, sawStart, sawListen, sawActivation bool
	var writeIdx, startIdx, listenIdx, activationIdx int
	for i, r := range recs {
		if r.Stage != trace.StageUnit && r.Stage != trace.StageActivation {
			continue
		}
		if r.Outcome != "ok" {
			t.Errorf("record %d outcome = %q, want ok: %+v", i, r.Outcome, r)
		}
		if r.Stage == trace.StageUnit {
			switch r.Detail["action"] {
			case "write":
				sawWrite, writeIdx = true, i
			case "start":
				sawStart, startIdx = true, i
			case "listen-proof":
				sawListen, listenIdx = true, i
			}
		}
		if r.Stage == trace.StageActivation {
			sawActivation, activationIdx = true, i
		}
	}
	if !sawWrite || !sawStart || !sawListen || !sawActivation {
		t.Fatalf("missing a unit/activation step; stages=%v", stages)
	}
	if !(writeIdx < startIdx && startIdx < listenIdx && listenIdx < activationIdx) {
		t.Fatalf("unit/activation steps out of order: write=%d start=%d listen=%d activation=%d",
			writeIdx, startIdx, listenIdx, activationIdx)
	}
}

// TestSetupLaneEmitsFailedUnitRecordWhenTheSupervisorRefuses: a step that
// fails must still be traced, with outcome=failed and the error, so a
// developer reading the trace after a failed install sees why rather than a
// silent gap where the sequence in the test above would otherwise stop.
func TestSetupLaneEmitsFailedUnitRecordWhenTheSupervisorRefuses(t *testing.T) {
	skipUnlessSupervised(t)
	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	defer restore()

	h := newLaneHarness(t)
	h.startFails = true
	a, _, _ := testApp(map[string]string{"HOME": h.home})

	if _, err := a.setupTelemetry(h.home, "127.0.0.1:18791", false); err == nil {
		t.Fatalf("setupTelemetry: want an error when the supervisor refuses the unit")
	}

	recs, _, err := trace.Read(dir, nil)
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	var sawFailedStart bool
	for _, r := range recs {
		if r.Stage == trace.StageUnit && r.Detail["action"] == "start" && r.Outcome == "failed" {
			sawFailedStart = true
			if r.Err == "" {
				t.Errorf("failed start record carries no Err: %+v", r)
			}
		}
	}
	if !sawFailedStart {
		t.Fatalf("no failed unit start record: %+v", recs)
	}
}

// TestRunDoctorEmitsOneDoctorFindingPerRow closes phase 3's other gap:
// `openbox doctor` printed a report but recorded none of it, so a developer
// could not later ask "what did doctor see" from the trace. A fresh,
// never-installed machine still has findings (identity, gateway, coverage),
// and every one of them must carry Stage=doctor.finding with a name and a
// status.
func TestRunDoctorEmitsOneDoctorFindingPerRow(t *testing.T) {
	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	defer restore()

	out, code := runDoctorIn(t, t.TempDir())
	if code != exitOK {
		t.Fatalf("doctor exit = %d:\n%s", code, out)
	}

	recs, skipped, err := trace.Read(dir, nil)
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	var findings int
	for _, r := range recs {
		// "output" records mirror doctor's printed lines
		// (TestRunDoctorTracesEveryOutputLine); this test is about the
		// structured findings.
		if r.Stage != trace.StageDoctor || r.Outcome == "output" {
			continue
		}
		findings++
		if r.Detail["name"] == nil || r.Detail["name"] == "" {
			t.Errorf("doctor.finding record has no name: %+v", r)
		}
		if r.Outcome == "" {
			t.Errorf("doctor.finding record %v has no status (Outcome)", r.Detail["name"])
		}
	}
	if findings == 0 {
		t.Fatalf("doctor produced no doctor.finding records at all: %+v", recs)
	}
}

// TestUninstallEmitsUninstallStepRecords: every surface `uninstall` reverses
// -- a hook registration, the settings restore, the credential/artifact
// deletes -- must leave a StageUninstall trail, so a developer can later see
// what a run actually removed from its own trace rather than only stdout.
func TestUninstallEmitsUninstallStepRecords(t *testing.T) {
	skipUnlessSupervised(t)
	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	defer restore()

	m := newInstalledMachine(t)
	a, out := m.app(t)
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("runUninstall: exit=%d:\n%s", code, out)
	}

	recs, skipped, err := trace.Read(dir, nil)
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	steps := map[string]bool{}
	for _, r := range recs {
		if r.Stage != trace.StageUninstall {
			continue
		}
		if r.Outcome == "" {
			t.Errorf("uninstall.step record has no Outcome: %+v", r)
		}
		if s, ok := r.Detail["step"].(string); ok {
			steps[s] = true
		}
	}
	for _, want := range []string{"hook-remove", "artifact-remove"} {
		if !steps[want] {
			t.Errorf("no uninstall.step record for %q; got steps=%v", want, steps)
		}
	}
}

// TestTraceTokenAcquisitionRecordsOutcomeNotToken: each workload-token
// acquisition becomes one auth record naming cached / fetched / failed.
func TestTraceTokenAcquisitionRecordsOutcomeNotToken(t *testing.T) {
	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	defer restore()

	traceTokenAcquisition(client.TokenAcquisition{FromCache: true})
	traceTokenAcquisition(client.TokenAcquisition{})
	traceTokenAcquisition(client.TokenAcquisition{Err: errors.New("exchange refused")})

	recs, _, err := trace.Read(dir, func(r trace.Record) bool { return r.Stage == trace.StageAuth })
	if err != nil || len(recs) != 3 {
		t.Fatalf("Read = %d, %v; want 3 auth records", len(recs), err)
	}
	for i, want := range []string{"token_cached", "token_fetched", "token_failed"} {
		if recs[i].Outcome != want {
			t.Errorf("record %d outcome = %q, want %q", i, recs[i].Outcome, want)
		}
	}
}

// TestRunDoctorTracesEveryOutputLine: doctor's report is its findings, so
// every line it prints is in the trace, not only the rows that also emit a
// structured finding.
func TestRunDoctorTracesEveryOutputLine(t *testing.T) {
	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	defer restore()

	out, _ := runDoctorIn(t, t.TempDir())
	recs, _, err := trace.Read(dir, func(r trace.Record) bool {
		return r.Stage == trace.StageDoctor && r.Outcome == "output"
	})
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	traced := map[string]bool{}
	for _, r := range recs {
		line, _ := r.Detail["line"].(string)
		traced[line] = true
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(line) != "" && !traced[line] {
			t.Errorf("doctor printed %q but the trace has no output record for it", line)
		}
	}
}
