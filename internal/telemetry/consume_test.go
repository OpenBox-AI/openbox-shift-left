package telemetry

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// These drive REAL pdata through the REAL consumers. No socket is involved, so
// they run everywhere; what they cannot cover is the HTTP path in front of them,
// which needs a listener this sandbox denies.

type captureEmitter struct {
	records []Record
	err     error
}

func (c *captureEmitter) Emit(_ context.Context, r Record) error {
	c.records = append(c.records, r)
	return c.err
}

// logsWith builds one resource/scope/record tree with the given attributes.
func logsWith(resource, scope, record map[string]string, ts time.Time) plog.Logs {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	putAll(rl.Resource().Attributes(), resource)
	sl := rl.ScopeLogs().AppendEmpty()
	putAll(sl.Scope().Attributes(), scope)
	lr := sl.LogRecords().AppendEmpty()
	putAll(lr.Attributes(), record)
	if !ts.IsZero() {
		lr.SetTimestamp(pcommon.NewTimestampFromTime(ts))
	}
	return ld
}

func putAll(m pcommon.Map, kv map[string]string) {
	for k, v := range kv {
		m.PutStr(k, v)
	}
}

func newTestReceiver(t *testing.T, e Emitter) *Receiver {
	t.Helper()
	r, err := New(Config{}, WithEmitter(e))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// The three attribute levels merge, and the most specific wins a collision.
// Without the precedence, a resource-level key would mask the per-record value
// that describes the event actually being reported.
func TestLogRecordMergesAttributesMostSpecificWins(t *testing.T) {
	cap := &captureEmitter{}
	r := newTestReceiver(t, cap)

	ts := time.Date(2026, 8, 28, 9, 20, 11, 0, time.UTC)
	ld := logsWith(
		map[string]string{"service.name": "claude-code", "shared": "from-resource"},
		map[string]string{"scope.only": "s", "shared": "from-scope"},
		map[string]string{eventNameAttr: "api_request", "shared": "from-record"},
		ts,
	)
	if err := r.consumeLogs(context.Background(), ld); err != nil {
		t.Fatalf("consumeLogs: %v", err)
	}

	if len(cap.records) != 1 {
		t.Fatalf("got %d records, want 1", len(cap.records))
	}
	rec := cap.records[0]
	if rec.Signal != SignalLogs {
		t.Errorf("Signal = %q, want %q", rec.Signal, SignalLogs)
	}
	if rec.EventName != "api_request" {
		t.Errorf("EventName = %q, want api_request", rec.EventName)
	}
	if !rec.Timestamp.Equal(ts) {
		t.Errorf("Timestamp = %v, want %v", rec.Timestamp, ts)
	}
	if got := rec.Attrs["shared"]; got != "from-record" {
		t.Errorf("shared = %q, want the record-level value to win", got)
	}
	for k, want := range map[string]string{"service.name": "claude-code", "scope.only": "s"} {
		if rec.Attrs[k] != want {
			t.Errorf("Attrs[%q] = %q, want %q", k, rec.Attrs[k], want)
		}
	}
}

// Sibling records must not see each other's attributes. The resource map is
// shared across every record beneath it, so a merge that wrote through the base
// would leak the first record's keys onto the second — and each record's
// attributes are what phase 10 attributes a turn from.
func TestSiblingRecordsDoNotShareAttributes(t *testing.T) {
	cap := &captureEmitter{}
	r := newTestReceiver(t, cap)

	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "claude-code")
	sl := rl.ScopeLogs().AppendEmpty()
	first := sl.LogRecords().AppendEmpty()
	first.Attributes().PutStr(eventNameAttr, "api_request")
	first.Attributes().PutStr("only.first", "1")
	second := sl.LogRecords().AppendEmpty()
	second.Attributes().PutStr(eventNameAttr, "tool_decision")

	if err := r.consumeLogs(context.Background(), ld); err != nil {
		t.Fatalf("consumeLogs: %v", err)
	}
	if len(cap.records) != 2 {
		t.Fatalf("got %d records, want 2", len(cap.records))
	}
	if _, leaked := cap.records[1].Attrs["only.first"]; leaked {
		t.Error("the second record carries the first's attribute; the merge wrote through the shared base")
	}
	if cap.records[1].Attrs["service.name"] != "claude-code" {
		t.Error("the second record lost the resource attribute it should inherit")
	}
}

// A record naming no event still reaches the emitter. Dropping it here would hide
// a provider rename as silence, and silence is the one signal OD4 turns into a
// finding — it must mean "the client sent nothing", not "we discarded it".
func TestUnnamedRecordStillDelivered(t *testing.T) {
	cap := &captureEmitter{}
	r := newTestReceiver(t, cap)
	if err := r.consumeLogs(context.Background(), logsWith(nil, nil, map[string]string{"a": "b"}, time.Time{})); err != nil {
		t.Fatalf("consumeLogs: %v", err)
	}
	if len(cap.records) != 1 {
		t.Fatalf("got %d records, want the unnamed record delivered", len(cap.records))
	}
	if cap.records[0].EventName != "" {
		t.Errorf("EventName = %q, want empty", cap.records[0].EventName)
	}
}

// The timestamp falls back to the observed time, and stays ZERO when neither is
// set. A fabricated `now` would be indistinguishable downstream from a real
// measurement, and this lane's whole value is that its records describe something
// that actually happened.
func TestTimestampFallsBackButIsNeverInvented(t *testing.T) {
	cap := &captureEmitter{}
	r := newTestReceiver(t, cap)

	observed := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	ld := plog.NewLogs()
	lr := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.SetObservedTimestamp(pcommon.NewTimestampFromTime(observed))
	if err := r.consumeLogs(context.Background(), ld); err != nil {
		t.Fatalf("consumeLogs: %v", err)
	}
	if !cap.records[0].Timestamp.Equal(observed) {
		t.Errorf("Timestamp = %v, want the observed time %v", cap.records[0].Timestamp, observed)
	}

	cap.records = nil
	if err := r.consumeLogs(context.Background(), logsWith(nil, nil, nil, time.Time{})); err != nil {
		t.Fatalf("consumeLogs: %v", err)
	}
	if !cap.records[0].Timestamp.IsZero() {
		t.Errorf("Timestamp = %v with neither time set, want the zero value", cap.records[0].Timestamp)
	}
}

// An emitter error must not propagate. This lane is additive by construction: a
// returned error becomes an export failure the governed tool retries and
// eventually surfaces, so a failing sink would degrade the very session it exists
// to observe silently.
func TestEmitterErrorNeverReachesTheExporter(t *testing.T) {
	cap := &captureEmitter{err: errFake{}}
	var warned int
	r, err := New(Config{}, WithEmitter(cap), WithWarnFunc(func(string, ...any) { warned++ }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.consumeLogs(context.Background(), logsWith(nil, nil, map[string]string{eventNameAttr: "api_request"}, time.Time{})); err != nil {
		t.Fatalf("consumeLogs returned %v; a failing emitter must not fail the export", err)
	}
	if warned == 0 {
		t.Error("a dropped record produced no warning; with launchd stdio at /dev/null this is the only signal there is")
	}
}

type errFake struct{}

func (errFake) Error() string { return "sink unavailable" }

// A receiver with no emitter still decodes and counts. Doctor's reachability
// probe and the install-time proof that the port is live both run before any
// emitter exists, so this path must not panic.
func TestNilEmitterStillCounts(t *testing.T) {
	r, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.consumeLogs(context.Background(), logsWith(nil, nil, map[string]string{eventNameAttr: "x"}, time.Time{})); err != nil {
		t.Fatalf("consumeLogs: %v", err)
	}
	if got := r.Counts()[SignalLogs]; got != 1 {
		t.Errorf("logs count = %d, want 1", got)
	}
}

// Counts answer "is it recording", which is what doctor needs to distinguish from
// "is it reachable" — the distinction the gateway lacks, where a working relay
// capturing nothing reads as healthy.
func TestCountsCoverEverySignal(t *testing.T) {
	cap := &captureEmitter{}
	r := newTestReceiver(t, cap)
	ctx := context.Background()

	if err := r.consumeLogs(ctx, logsWith(nil, nil, map[string]string{eventNameAttr: "a"}, time.Time{})); err != nil {
		t.Fatal(err)
	}
	td := ptrace.NewTraces()
	td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	if err := r.consumeTraces(ctx, td); err != nil {
		t.Fatal(err)
	}
	md := pmetric.NewMetrics()
	g := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	g.SetEmptyGauge().DataPoints().AppendEmpty()
	if err := r.consumeMetrics(ctx, md); err != nil {
		t.Fatal(err)
	}

	counts := r.Counts()
	for sig, want := range map[Signal]int64{SignalLogs: 1, SignalTraces: 1, SignalMetrics: 1} {
		if counts[sig] != want {
			t.Errorf("%s count = %d, want %d", sig, counts[sig], want)
		}
	}

	// Traces and metrics are accepted and discarded, not projected — phase 10
	// decides whether either carries anything the contract wants. They must not
	// reach the emitter as if they had been mapped.
	if len(cap.records) != 1 {
		t.Errorf("emitter saw %d records, want only the log record", len(cap.records))
	}
}

// An oversized attribute is TRUNCATED, not dropped, and truncation lands on a
// rune boundary. A value cut mid-sequence becomes invalid UTF-8, which
// json.Marshal rewrites to U+FFFD on the way to the spool — turning a clipped
// value into a corrupted one.
func TestOversizedAttributeIsTruncatedOnARuneBoundary(t *testing.T) {
	cap := &captureEmitter{}
	r := newTestReceiver(t, cap)

	// Multi-byte runes, so a byte-wise cut would land mid-sequence.
	huge := strings.Repeat("é", maxAttrValueBytes)
	if err := r.consumeLogs(context.Background(), logsWith(nil, nil, map[string]string{"big": huge}, time.Time{})); err != nil {
		t.Fatalf("consumeLogs: %v", err)
	}
	got := cap.records[0].Attrs["big"]
	if len(got) > maxAttrValueBytes {
		t.Errorf("value is %d bytes, over the %d bound", len(got), maxAttrValueBytes)
	}
	if got == "" {
		t.Fatal("value was dropped; it should be truncated — a value that arrived is evidence even clipped")
	}
	if !utf8Valid(got) {
		t.Error("truncation produced invalid UTF-8; json.Marshal would rewrite it to U+FFFD")
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// The attribute count is bounded. MaxRequestBodyBytes bounds the REQUEST; without
// this a single conforming request could still expand into a map large enough to
// matter, and the spool would then carry it.
func TestAttributeCountIsBounded(t *testing.T) {
	cap := &captureEmitter{}
	r := newTestReceiver(t, cap)

	many := make(map[string]string, maxAttrs*2)
	for i := 0; i < maxAttrs*2; i++ {
		many[string(rune('a'+i%26))+strings.Repeat("x", i)] = "v"
	}
	if err := r.consumeLogs(context.Background(), logsWith(nil, nil, many, time.Time{})); err != nil {
		t.Fatalf("consumeLogs: %v", err)
	}
	if got := len(cap.records[0].Attrs); got > maxAttrs {
		t.Errorf("record carries %d attributes, over the %d bound", got, maxAttrs)
	}
}
