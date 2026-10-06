package main

import (
	"bytes"
	"io"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// This file is the shared, tiny instrumentation seam every process-edge in
// this package (init, doctor, uninstall) emits through, so a step's shape on
// disk stays identical regardless of which file called it. It never blocks or
// fails a step: trace.Emit already swallows every write error, so recording
// here is strictly additive to whatever the caller was already doing.

// outcomeOf renders ok/failed off whether err is nil, which is the one
// vocabulary every StageUnit/StageActivation/StageUninstall record in this
// package uses for its Outcome field (StageDoctor's Outcome is a caller-given
// status word instead, since a finding is not a pass/fail step).
func outcomeOf(err error) string {
	if err != nil {
		return "failed"
	}
	return "ok"
}

// mergeDetail copies extra over a name/value seed so a caller-supplied
// detail map can never clobber the record's own identifying field, and a nil
// extra (the common case) costs nothing beyond the seed itself.
func mergeDetail(seed map[string]any, extra map[string]any) map[string]any {
	for k, v := range extra {
		seed[k] = v
	}
	return seed
}

// traceUnit records one step of a lane unit's lifecycle: write, start
// (load), listen-proof, or remove. lane is the daemon this step is about
// ("telemetry", "transport", "gateway"); action names the step itself.
func traceUnit(lane, action string, err error, detail map[string]any) {
	rec := trace.Record{
		Stage:   trace.StageUnit,
		Outcome: outcomeOf(err),
		Detail:  mergeDetail(map[string]any{"lane": lane, "action": action}, detail),
	}
	if err != nil {
		rec.Err = err.Error()
	}
	trace.Emit(rec)
}

// traceActivation records one privileged activation step (CA trust, its
// read-back, a PAC write, an env-key activation) with whatever prior value
// this step displaced, for a reconciler that needs to know what was there
// before OpenBox touched it. prior is nil when the step never had one (a
// fresh machine, or a step that captures no prior value of its own).
func traceActivation(step string, err error, prior any, detail map[string]any) {
	d := mergeDetail(map[string]any{"step": step}, detail)
	if prior != nil {
		d["prior"] = prior
	}
	rec := trace.Record{Stage: trace.StageActivation, Outcome: outcomeOf(err), Detail: d}
	if err != nil {
		rec.Err = err.Error()
	}
	trace.Emit(rec)
}

// traceDoctorFinding records one row of `openbox doctor`'s report: name is
// the row's label, status is its own short answer word (already computed by
// the caller, since doctor's status vocabulary differs finding to finding),
// message is the human-readable detail already printed to stdout.
func traceDoctorFinding(name, status, message string) {
	trace.Emit(trace.Record{
		Stage:   trace.StageDoctor,
		Outcome: status,
		Detail:  map[string]any{"name": name, "message": message},
	})
}

// traceUninstallStep records one step of `openbox uninstall`'s reversal:
// removing a hook surface, restoring settings, flushing a spool, deleting an
// artifact, reverting the system PAC/CA trust.
func traceUninstallStep(step string, err error, detail map[string]any) {
	rec := trace.Record{
		Stage:   trace.StageUninstall,
		Outcome: outcomeOf(err),
		Detail:  mergeDetail(map[string]any{"step": step}, detail),
	}
	if err != nil {
		rec.Err = err.Error()
	}
	trace.Emit(rec)
}

// traceTokenAcquisition is the client.SetTokenObserver callback main
// installs: one auth record per workload-token acquisition, in every process
// (hook or daemon), so a cold exchange or a refresh failure is visible next
// to the delivery it delayed or failed. The token itself never reaches it.
func traceTokenAcquisition(a client.TokenAcquisition) {
	r := trace.Record{
		Stage:   trace.StageAuth,
		Outcome: "token_fetched",
		DurMS:   float64(a.Duration) / float64(time.Millisecond),
	}
	switch {
	case a.Err != nil:
		r.Outcome = "token_failed"
		r.ErrClass = client.FailureClass(a.Err)
		r.Err = a.Err.Error()
	case a.FromCache:
		r.Outcome = "token_cached"
	}
	trace.Emit(r)
}

// doctorOutputTee passes doctor's output through unchanged and records each
// completed line as a doctor.finding "output" record; flush records a final
// line with no trailing newline.
type doctorOutputTee struct {
	w       io.Writer
	pending []byte
}

func (t *doctorOutputTee) Write(p []byte) (int, error) {
	t.pending = append(t.pending, p...)
	for {
		i := bytes.IndexByte(t.pending, '\n')
		if i < 0 {
			break
		}
		t.record(string(t.pending[:i]))
		t.pending = t.pending[i+1:]
	}
	return t.w.Write(p)
}

func (t *doctorOutputTee) flush() {
	if len(t.pending) > 0 {
		t.record(string(t.pending))
		t.pending = nil
	}
}

func (t *doctorOutputTee) record(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	trace.Emit(trace.Record{
		Stage:   trace.StageDoctor,
		Outcome: "output",
		Detail:  map[string]any{"line": line},
	})
}
