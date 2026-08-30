package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// Emitter receives normalized records. This module must not grow a transport
// or read a credential; its own guard test forbids both; so the seam is where
// the dependency inverts.
type Emitter interface {
	// Emit is called once per decoded record. It must not block the caller for
	// long: it runs on the receiver's request goroutine, and a slow emitter
	// becomes backpressure on the governed tool's own export.
	Emit(ctx context.Context, r Record) error
}

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

func (r *Receiver) consumeTraces(_ context.Context, td ptrace.Traces) error {
	r.counts.add(SignalTraces, int64(td.SpanCount()))
	return nil
}

func (r *Receiver) consumeMetrics(_ context.Context, md pmetric.Metrics) error {
	r.counts.add(SignalMetrics, int64(md.DataPointCount()))
	return nil
}

// deliver hands one record to the emitter and records that it arrived.
func (r *Receiver) deliver(ctx context.Context, rec Record) {
	r.counts.add(rec.Signal, 1)
	if r.emitter == nil {
		return
	}
	if err := r.emitter.Emit(ctx, rec); err != nil {
		r.warnf("telemetry: emit %s/%s: %v", rec.Signal, rec.EventName, err)
	}
}

func eventName(attrs pcommon.Map) string {
	if v, ok := attrs.Get(eventNameAttr); ok {
		return truncate(v.AsString())
	}
	return ""
}

func recordTime(ts, observed pcommon.Timestamp) time.Time {
	if ts != 0 {
		return ts.AsTime()
	}
	if observed != 0 {
		return observed.AsTime()
	}
	return time.Time{}
}

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

func truncate(s string) string {
	if len(s) <= maxAttrValueBytes {
		return s
	}
	b := []byte(s[:maxAttrValueBytes])
	for len(b) > 0 && !utf8Start(b[len(b)-1]) && !utf8Single(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	if len(b) > 0 && utf8Start(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return string(b)
}

func utf8Single(b byte) bool { return b&0x80 == 0 }
func utf8Start(b byte) bool  { return b&0xc0 == 0xc0 }
