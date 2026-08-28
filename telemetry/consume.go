package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// Emitter receives normalized records. The receiver holds one and calls it for
// every record it decodes.
//
// It is an interface for one reason, and it is not testability: the emitter that
// matters lives in the CLI, where the client, credentials and spool already are,
// exactly as the gateway's does. This module must not grow a transport or read a
// credential — its own guard test forbids both — so the seam is where the
// dependency inverts.
//
// The gateway shipped with this seam UNWIRED: WithCapture had no production
// caller, so every capture was discarded while package tests passed against a
// stub emitter on one side and a hand-written event on the other. A fake at each
// end of a seam with no implementation between them proves nothing about the
// seam. Whatever wires this one owes a control test that uses neither.
type Emitter interface {
	// Emit is called once per decoded record. It must not block the caller for
	// long: it runs on the receiver's request goroutine, and a slow emitter
	// becomes backpressure on the governed tool's own export.
	//
	// An error is logged and dropped, never returned to the exporter. This lane
	// is ADDITIVE BY CONSTRUCTION: telemetry that starts failing must never
	// degrade the session it observes, so a rejected export — which the tool
	// would retry, and eventually surface — is a worse failure than a lost
	// record. OD4's silence-is-a-finding is the compensating control.
	Emit(ctx context.Context, r Record) error
}

// consumeLogs is the logs consumer. Claude Code's enhanced telemetry carries its
// events here — api_request, tool_decision, user_prompt and the rest — so this is
// the signal the lane is really about.
func (r *Receiver) consumeLogs(ctx context.Context, ld plog.Logs) error {
	for i := 0; i < ld.ResourceLogs().Len(); i++ {
		rl := ld.ResourceLogs().At(i)
		base := attrsOf(rl.Resource().Attributes(), nil)
		for j := 0; j < rl.ScopeLogs().Len(); j++ {
			sl := rl.ScopeLogs().At(j)
			withScope := attrsOf(sl.Scope().Attributes(), base)
			for k := 0; k < sl.LogRecords().Len(); k++ {
				lr := sl.LogRecords().At(k)
				r.deliver(ctx, Record{
					Signal:    SignalLogs,
					EventName: eventName(lr.Attributes()),
					Timestamp: recordTime(lr.Timestamp(), lr.ObservedTimestamp()),
					Attrs:     attrsOf(lr.Attributes(), withScope),
				})
			}
		}
	}
	return nil
}

// consumeTraces and consumeMetrics accept and count their signals without
// projecting a per-record shape yet.
//
// The endpoints are enabled because a receiver that 404s a signal the exporter
// was configured to send produces client-side export errors in the governed
// tool's own logs — a lane that is supposed to be invisible making noise in the
// thing it observes. Accepting and discarding is the quiet behaviour, and the
// counters are what let doctor tell reachable-but-silent from recording.
//
// Phase 10 decides whether either carries anything this contract wants. Binding
// fields here first would be guessing at a projection before its consumer exists.
func (r *Receiver) consumeTraces(_ context.Context, td ptrace.Traces) error {
	r.counts.add(SignalTraces, int64(td.SpanCount()))
	return nil
}

func (r *Receiver) consumeMetrics(_ context.Context, md pmetric.Metrics) error {
	r.counts.add(SignalMetrics, int64(md.DataPointCount()))
	return nil
}

// deliver hands one record to the emitter and records that it arrived.
//
// The count is incremented whether or not the emitter succeeds, deliberately: it
// answers "is this lane receiving", which is the question doctor asks and the one
// OD4 turns into a finding. Whether the record then reached the spool is the
// emitter's own business and its own log line; conflating the two would let a
// broken emitter read as a silent client.
func (r *Receiver) deliver(ctx context.Context, rec Record) {
	r.counts.add(rec.Signal, 1)
	if r.emitter == nil {
		return
	}
	if err := r.emitter.Emit(ctx, rec); err != nil {
		// Never the record's attributes — they carry prompts and tool output.
		// Same rule the gateway's verbose logging follows: shape, not content.
		r.warnf("telemetry: emit %s/%s: %v", rec.Signal, rec.EventName, err)
	}
}

// eventName reads the provider's event discriminator, or "" when absent.
func eventName(attrs pcommon.Map) string {
	if v, ok := attrs.Get(eventNameAttr); ok {
		return truncate(v.AsString())
	}
	return ""
}

// recordTime prefers the record's own timestamp and falls back to the observed
// one. Zero when neither is set — a fabricated `now` here would be indistinguishable
// from a real measurement downstream, and this lane's whole value is that its
// records describe something that happened.
func recordTime(ts, observed pcommon.Timestamp) time.Time {
	if ts != 0 {
		return ts.AsTime()
	}
	if observed != 0 {
		return observed.AsTime()
	}
	return time.Time{}
}

// attrsOf flattens one attribute map over an inherited base.
//
// Order is resource → scope → record, so the most specific level wins a key
// collision. The base is copied rather than mutated: sibling records share a
// resource map, and writing through would leak one record's attributes onto the
// next.
func attrsOf(m pcommon.Map, base map[string]string) map[string]string {
	out := make(map[string]string, m.Len()+len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range m.All() {
		if len(out) >= maxAttrs {
			if _, replacing := out[k]; !replacing {
				break
			}
		}
		out[k] = truncate(v.AsString())
	}
	return out
}

// truncate bounds one attribute value.
func truncate(s string) string {
	if len(s) <= maxAttrValueBytes {
		return s
	}
	// Cut on a rune boundary: a value sliced mid-sequence becomes invalid UTF-8,
	// which json.Marshal rewrites to U+FFFD — turning a clipped value into a
	// corrupted one on the way to the spool.
	b := []byte(s[:maxAttrValueBytes])
	for len(b) > 0 && !utf8Start(b[len(b)-1]) && !utf8Single(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	if len(b) > 0 && utf8Start(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return string(b)
}

// utf8Single reports an ASCII byte; utf8Start reports a multi-byte sequence's
// leading byte. Continuation bytes are neither.
func utf8Single(b byte) bool { return b&0x80 == 0 }
func utf8Start(b byte) bool  { return b&0xc0 == 0xc0 }
