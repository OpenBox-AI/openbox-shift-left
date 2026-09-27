package hookflow

import (
	"context"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// This file wires internal/trace (the local, full-fidelity record) into
// this package's own delivery, spool, lane-queue, latch and gate-verdict
// paths. Every call here MUST stay off the blocking/failing path: trace.Emit
// itself never blocks long or panics (see internal/trace), and every helper
// below only ever builds a Record and calls it -- nothing here can fail a
// tool call or a delivery.
//
// internal/trace has zero repo imports, so importing it here (and from the
// claude-code/codex adapter packages) creates no cycle; it is not one of the
// four dependency-guarded subtrees (internal/decision, internal/telemetry,
// internal/transport, internal/gateway) either, so no depguard entry is
// needed.

// providerOf reads the provider name every adapter's own Mapper already
// stamps onto Metadata["provider"] (claude-code/mapper.go,
// codex/mapper.go), so this shared package can label a trace record without
// importing either adapter package -- which would cycle, since both import
// hookflow.
func providerOf(ev client.DevEvent) string {
	if s, ok := ev.Metadata["provider"].(string); ok {
		return s
	}
	return ""
}

// traceEventFields fills a trace.Record's event identifiers from ev, wherever
// known. A record built this way never carries ActivityID (computed deep in
// internal/client, unexported there) or Lane (a lane-daemon-only concept this
// event's own producer may not have); a caller that DOES know either sets it
// itself.
func traceEventFields(r trace.Record, ev client.DevEvent) trace.Record {
	r.Provider = providerOf(ev)
	r.SessionID = ev.SessionID
	r.RunID = ev.RunID
	r.ActivityID = client.WireActivityID(ev)
	r.Lane = laneOf(ev)
	r.EventID = ev.EventID
	r.EventType = string(ev.EventType)
	return r
}

// laneOf names the producer of ev by the discriminator each lane stamps:
// one of the three model-call lanes, else the hook path that every other
// event comes from.
func laneOf(ev client.DevEvent) string {
	switch {
	case ev.ProxyRequestID != "":
		return "proxy"
	case ev.GatewayRequestID != "":
		return "gateway"
	case ev.OtelRequestID != "":
		return "otel"
	}
	return "hook"
}

type deliveryAttemptKey struct{}

// WithDeliveryAttempt marks ctx as carrying delivery attempt n of one event,
// so Deliver's trace records say which try they describe. A retry site
// passes 2; an unmarked ctx is attempt 1. It changes nothing but the trace.
func WithDeliveryAttempt(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, deliveryAttemptKey{}, n)
}

func deliveryAttempt(ctx context.Context) int {
	if n, ok := ctx.Value(deliveryAttemptKey{}).(int); ok && n > 0 {
		return n
	}
	return 1
}

// durMS reports the elapsed time since start in fractional milliseconds, the
// unit trace.Record.DurMS is documented in.
func durMS(start time.Time) float64 {
	return float64(time.Since(start)) / float64(time.Millisecond)
}

// errString reports err.Error(), or "" for a nil err -- trace.Record.Err is
// omitempty, so "" and absent read the same way to a reader.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// traceDeliverAttempt records one delivery attempt BEFORE it is made: the
// egressed (already-redacted) event this attempt is about to send, and which
// attempt it is (WithDeliveryAttempt).
func traceDeliverAttempt(ev client.DevEvent, attempt int) {
	trace.Emit(traceEventFields(trace.Record{
		Stage:   trace.StageDeliverAttempt,
		Attempt: attempt,
		Detail:  map[string]any{"event": trace.JSON(ev)},
	}, ev))
}

// traceDeliverResult records one delivery attempt's outcome: the same
// egressed event, the verdict (when core answered), the failure class and
// whether it earns a retry (client.FailureClass/RetryableDelivery), and the
// attempt's own duration.
func traceDeliverResult(ev client.DevEvent, attempt int, eval client.Evaluation, err error, dur time.Duration) {
	r := traceEventFields(trace.Record{
		Stage:   trace.StageDeliverResult,
		Attempt: attempt,
		DurMS:   float64(dur) / float64(time.Millisecond),
	}, ev)
	detail := map[string]any{
		"event":     trace.JSON(ev),
		"verdict":   string(eval.Verdict),
		"reason":    eval.Reason,
		"retryable": client.RetryableDelivery(err),
	}
	if err != nil {
		r.ErrClass = client.FailureClass(err)
		r.Err = errString(err)
		r.Outcome = "error"
	} else {
		r.Outcome = "accepted"
	}
	r.Detail = detail
	trace.Emit(r)
}

// traceSpoolAppend records one event durably appended to its session's spool
// (Spool.Append's own success path only -- a write failure is already
// surfaced to the caller as an error and never silently retried).
func traceSpoolAppend(ev client.DevEvent) {
	trace.Emit(traceEventFields(trace.Record{Stage: trace.StageSpoolAppend, Outcome: "ok"}, ev))
}

// traceSpoolDiscard records a batch of spooled events the spool gave up on
// (Spool.recordDiscard's own callers: a single-attempt failure, a corrupt
// line, a legacy carry-over file, a crash orphan). name is the session stem
// the discard ledger already keys on.
func traceSpoolDiscard(name string, events int, reason string) {
	trace.Emit(trace.Record{
		SessionID: name,
		Stage:     trace.StageSpoolDiscard,
		Outcome:   "discarded",
		Detail:    map[string]any{"events": events, "reason": reason},
	})
}

// traceSpoolRetire records a spool file removed for being past its retention
// age, never delivered (Spool.RetireStale).
func traceSpoolRetire(name string, events int, ageDays int) {
	trace.Emit(trace.Record{
		SessionID: name,
		Stage:     trace.StageSpoolRetire,
		Outcome:   "retired",
		Detail:    map[string]any{"events": events, "age_days": ageDays},
	})
}

// tracePoolDrop records one DeliverPool record that never reached core:
// either Submit refused it outright (the pool was closed or saturated) or a
// drain's own tail was dropped after an earlier record in the same session
// was not accepted.
func tracePoolDropSubmit(ev client.DevEvent, reason string) {
	trace.Emit(traceEventFields(trace.Record{Stage: trace.StagePoolDrop, Outcome: reason}, ev))
}

func tracePoolDropTail(sessionID string, count int) {
	if count <= 0 {
		return
	}
	trace.Emit(trace.Record{
		SessionID: sessionID,
		Stage:     trace.StagePoolDrop,
		Outcome:   "tail_drop_after_refusal",
		Detail:    map[string]any{"count": count},
	})
}

// traceQueueAbandon records one DeliverPool record Close abandoned: accepted
// by Submit, but never given even one attempt before ctx's own deadline.
func traceQueueAbandon(ev client.DevEvent) {
	trace.Emit(traceEventFields(trace.Record{Stage: trace.StageQueueAbandon, Outcome: "closed_before_attempt"}, ev))
}

// traceLaneDrain records one LaneQueue drain pass: DrainSession's own
// stripe-wait-then-attempt cycle, from this queue's point of view (the
// per-session lock acquisition itself is separately traced in
// sessionlock.go's own StageStripeWait record).
func traceLaneDrain(sessionID string, start time.Time, delivered int, err error) {
	trace.Emit(trace.Record{
		SessionID: sessionID,
		Stage:     trace.StageStripeWait,
		Outcome:   "lane_drain_pass",
		DurMS:     durMS(start),
		Err:       errString(err),
		Detail:    map[string]any{"delivered": delivered},
	})
}

// traceStripeWait records one session stripe lock acquisition attempt
// (sessionlock.go's lockSession), so a caller waiting behind a live drainer
// (MaxStripeWait, a gate's own drain step) is visible in the trace.
func traceStripeWait(sessionID string, mode DrainMode, start time.Time, outcome string, err error) {
	trace.Emit(trace.Record{
		SessionID: sessionID,
		Stage:     trace.StageStripeWait,
		Outcome:   outcome,
		DurMS:     durMS(start),
		Err:       errString(err),
		Detail:    map[string]any{"mode": mode == Block},
	})
}

// traceLatchSet records a run being latched -- halted, either by a live HALT
// verdict (WriteSessionHalt) or by an unaccepted event/orphaned drain
// (WriteSessionHaltIfAbsent, HaltOnDeliveryFailure) -- the moment the latch
// file was actually (newly) written. runID is the run this Reason/Cause now
// answers every later gated call for.
func traceLatchSet(runID string, info SessionHaltInfo) {
	trace.Emit(trace.Record{
		RunID:     runID,
		SessionID: runID,
		Stage:     trace.StageLatchSet,
		Outcome:   "latched",
		Detail: map[string]any{
			"reason":     info.Reason,
			"policy_id":  info.PolicyID,
			"cause":      info.Cause,
			"event_type": info.EventType,
		},
	})
}

// traceLocalRedaction records the local (never-egressed) secret-redaction
// step: the raw content BEFORE redaction and the result AFTER it, plus the
// content-free category names that fired. Recorded regardless of
// content_capture (the trace captures everything), unlike
// the durable enforcement audit (EnforcementRecord), which only ever carries
// the category names.
func traceLocalRedaction(sessionID string, eventType client.EventType, before string, after *client.Content, categories []string) {
	detail := map[string]any{
		"categories": categories,
		"changed":    after != nil,
	}
	if before != "" {
		detail["before"] = trace.Body(before)
	}
	if after != nil {
		detail["after"] = trace.Body(after.FileText)
	}
	trace.Emit(trace.Record{
		SessionID: sessionID,
		EventType: string(eventType),
		Stage:     trace.StageDecision,
		Outcome:   "redaction",
		Detail:    detail,
	})
}

// traceGateVerdict records the local gate's own applied decision: the
// evaluation this call actually rendered onto the provider's hook contract,
// and how (RecordEnforcement's own callers, shared by every gated hook class
// across both providers).
func traceGateVerdict(sessionID, toolKind string, verdict string, source string, failOpen bool, appliedDecision string, redacted bool, categories []string, policyID string) {
	trace.Emit(trace.Record{
		SessionID: sessionID,
		Stage:     trace.StageGateVerdict,
		Outcome:   verdict,
		Detail: map[string]any{
			"tool_kind":            toolKind,
			"source":               source,
			"fail_open":            failOpen,
			"applied_decision":     appliedDecision,
			"redacted":             redacted,
			"redaction_categories": categories,
			"policy_id":            policyID,
		},
	})
}

// TraceHookIn records a hook's raw stdin -- BEFORE the payload is parsed, and
// even when parsing fails: the contract is that a malformed payload
// is still fully traced. sessionID is "" when parsing failed before a session
// id could be read.
func TraceHookIn(providerName, hook, sessionID string, raw []byte, err error) {
	r := trace.Record{
		Provider:  providerName,
		SessionID: sessionID,
		EventType: hook,
		Stage:     trace.StageHookIn,
		Detail:    map[string]any{"stdin": trace.Body(string(raw))},
	}
	if err != nil {
		r.Err = errString(err)
		r.Outcome = "parse_error"
	} else {
		r.Outcome = "ok"
	}
	trace.Emit(r)
}

// HookOutputBuffer is the copy of a hook's stdout its hook.out record is
// built from. It keeps at most trace.MaxBodyBytes -- more could never be
// stored -- so a verbose hook cannot grow the process's memory without
// bound, and it never fails a Write: the real stdout, not this copy, is what
// the tool reads.
type HookOutputBuffer struct {
	kept  []byte
	total int
}

// Write keeps what still fits under the cap and counts the rest.
//
// It reports the whole of p written: io.MultiWriter treats anything less as
// a short write and would fail the hook's real stdout with it.
func (b *HookOutputBuffer) Write(p []byte) (int, error) {
	b.total += len(p)
	if room := trace.MaxBodyBytes - len(b.kept); room > 0 {
		b.kept = append(b.kept, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

// Bytes is the kept prefix of the hook's stdout.
func (b *HookOutputBuffer) Bytes() []byte { return b.kept }

// Total is how many bytes the hook wrote, kept or not.
func (b *HookOutputBuffer) Total() int { return b.total }

// TraceHookOut records a hook's stdout and the whole hook's own wall-clock
// duration (measured from the hook process's own start, not from wherever
// the deferred call wiring this happens to sit).
func TraceHookOut(providerName, hook, sessionID string, start time.Time, stdout *HookOutputBuffer) {
	trace.Emit(trace.Record{
		Provider:  providerName,
		SessionID: sessionID,
		EventType: hook,
		Stage:     trace.StageHookOut,
		DurMS:     durMS(start),
		Detail: map[string]any{
			"stdout":       trace.Body(string(stdout.Bytes())),
			"stdout_bytes": stdout.Total(),
		},
	})
}
