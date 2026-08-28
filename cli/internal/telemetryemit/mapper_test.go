package telemetryemit

import (
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/telemetry"
)

const testDID = "did:aip:openbox:dev:mapper-test"

// apiRequest builds the corpus's api_request shape. The attribute names, and the
// fact every value arrives as a STRING, come from a real desktop export
// (claude-code-desktop 1.37937.3, run 20260827T063932Z-225cac) — see
// reports/measure-260828-otel-attribute-inventory.md.
//
// The string-typed values are not a simplification: consume.go flattens every
// OTLP value type through AsString, deliberately, because the provider types the
// SAME attribute differently per event (duration_ms is intValue on api_request
// and stringValue on tool_result). A mapper that parsed from typed values would
// read zero on one of them, silently.
func apiRequest(overrides map[string]string) telemetry.Record {
	attrs := map[string]string{
		"event.name":            "api_request",
		"session.id":            "sess-1",
		"model":                 "claude-opus-4-8",
		"input_tokens":          "2",
		"output_tokens":         "173",
		"cache_read_tokens":     "90485",
		"cache_creation_tokens": "333",
		"duration_ms":           "4210",
		"cost_usd":              "0.0123",
		"request_id":            "req_011CeSoFqW2HfEh9jxCds86Y",
		"client_request_id":     "b3f1c2d4-0000-4000-8000-000000000001",
	}
	for k, v := range overrides {
		if v == "" {
			delete(attrs, k)
			continue
		}
		attrs[k] = v
	}
	return telemetry.Record{
		Signal:    telemetry.SignalLogs,
		EventName: attrs["event.name"],
		Timestamp: time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC),
		Attrs:     attrs,
	}
}

func elected() *Mapper {
	return New(testDID, Policy{Elected: true})
}

// TestUnelectedMapperEmitsNothing is the single most important test in the file.
//
// Two lanes can describe the same model call. If both emit, core does NOT absorb
// one as a duplicate — the namespaces are disjoint by design — so the dashboards
// simply double every token count, with no error anywhere. The election (phase
// 12) is what makes exactly one lane emit, and it does not exist yet.
//
// So the zero value of Policy SUPPRESSES. A partially-built lane cannot
// double-count in the field, because the only policy anyone can construct today
// without saying "Elected" out loud is the one that emits nothing.
func TestUnelectedMapperEmitsNothing(t *testing.T) {
	m := New(testDID, Policy{})
	for _, name := range []string{"api_request", "api_response_body", "tool_result", "tool_decision", "user_prompt"} {
		rec := apiRequest(map[string]string{"event.name": name})
		rec.EventName = name
		if ev, ok := m.EventFor(rec); ok {
			t.Errorf("%s: an UNELECTED mapper emitted %s — this doubles every token count on every dashboard", name, ev.EventType)
		}
	}
}

func TestAPIRequestBecomesTurnCompleted(t *testing.T) {
	ev, ok := elected().EventFor(apiRequest(nil))
	if !ok {
		t.Fatal("api_request produced no event")
	}

	// TurnCompleted is not a choice: buildPayload attaches an observed span only
	// under that case, and gatewayemit.EventFor takes the same close-only shape
	// (legitimised by v1.6's $defs.turnProducer repair). A pair would need
	// turn_index or a second discriminator on the opening half.
	if ev.EventType != client.EventTurnCompleted {
		t.Errorf("event type = %s, want TurnCompleted", ev.EventType)
	}
	if ev.SchemaVersion != client.SchemaVersion {
		t.Errorf("schema version = %q, want %q", ev.SchemaVersion, client.SchemaVersion)
	}
	if ev.SessionID != "sess-1" {
		t.Errorf("session = %q, want sess-1 (from the record's session.id, which is RECORD-level not resource-level)", ev.SessionID)
	}
	if ev.DeveloperDID != testDID {
		t.Errorf("did = %q", ev.DeveloperDID)
	}
	if ev.Model != "claude-opus-4-8" {
		t.Errorf("model = %q — this is core's aggregation key", ev.Model)
	}

	// The lane discriminator. Without it turnActivityIDFor falls through to the
	// hook path's TurnIndex branch and, with no index, returns an EMPTY
	// activity_id.
	if ev.OtelRequestID != "req_011CeSoFqW2HfEh9jxCds86Y" {
		t.Errorf("otel_request_id = %q, want the provider's request_id", ev.OtelRequestID)
	}
	if ev.GatewayRequestID != "" || ev.ProxyRequestID != "" {
		t.Error("an otel event set another lane's discriminator — the contract's turnProducer oneOf requires exactly one")
	}

	if ev.Tokens == nil {
		t.Fatal("no tokens — the whole point of this lane")
	}
	// input_tokens is PURE input. Measured on the corpus: input=2 alongside
	// cache_read=90485, so it excludes cache, which is exactly contract v1.1's
	// redefinition. Summing them here would double-count ~90k tokens per call.
	for _, c := range []struct {
		name string
		got  *int
		want int
	}{
		{"input", ev.Tokens.Input, 2},
		{"output", ev.Tokens.Output, 173},
		{"cache_read", ev.Tokens.CacheRead, 90485},
		{"cache_creation_input", ev.Tokens.CacheCreationInput, 333},
		{"total", ev.Tokens.Total, 2 + 173 + 90485 + 333},
	} {
		if c.got == nil {
			t.Errorf("%s is nil, want %d", c.name, c.want)
			continue
		}
		if *c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, *c.got, c.want)
		}
	}

	// The span exists for exactly one reason: core RECOMPUTES semantic_type, and
	// isLLMCall's attribute inputs are the only path to llm_completion. Without
	// it the activity stores and every model-call reader goes quiet.
	if ev.Span == nil {
		t.Fatal("no span — core cannot classify this as llm_completion without one")
	}
	if ev.Span.SemanticType != "llm_completion" {
		t.Errorf("span semantic_type = %q", ev.Span.SemanticType)
	}
}

// TestOtelRequestIDRejectsMalformedProviderValues guards event IDENTITY.
//
// OtelRequestID becomes part of activity_id, which is this product's event
// identity and is byte-pinned and load-bearing for core's dedupe. A provider
// value reaches it straight off the wire, so it is bounded and charset-checked
// first. ':' matters most: activity_id is "<session>:otel:<id>", and a colon
// inside the id makes the namespace ambiguous.
//
// The safe direction is to emit NOTHING rather than an event with a malformed
// identity — a dropped turn is a gap, a colliding activity_id corrupts a row.
func TestOtelRequestIDRejectsMalformedProviderValues(t *testing.T) {
	cases := map[string]string{
		"a colon breaks the namespace": "req:with:colons",
		"whitespace":                   "req with spaces",
		"a newline":                    "req\nid",
		"over the length bound":        strings.Repeat("a", 200),
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			// Both id sources bad, so there is nothing legitimate to fall back to.
			rec := apiRequest(map[string]string{"request_id": bad, "client_request_id": bad})
			if ev, ok := elected().EventFor(rec); ok {
				t.Errorf("emitted an event with otel_request_id %q (activity_id would be %q…)", ev.OtelRequestID, ev.SessionID+":otel:"+ev.OtelRequestID)
			}
		})
	}
}

func TestOtelRequestIDFallsBackToClientRequestID(t *testing.T) {
	rec := apiRequest(map[string]string{"request_id": ""})
	ev, ok := elected().EventFor(rec)
	if !ok {
		t.Fatal("no event; client_request_id should have served as the id")
	}
	if ev.OtelRequestID != "b3f1c2d4-0000-4000-8000-000000000001" {
		t.Errorf("otel_request_id = %q, want the client_request_id", ev.OtelRequestID)
	}
}

func TestNoIDAtAllEmitsNothing(t *testing.T) {
	rec := apiRequest(map[string]string{"request_id": "", "client_request_id": ""})
	if _, ok := elected().EventFor(rec); ok {
		t.Error("emitted a turn with no provider id — a minted id would break idempotency across a re-flush")
	}
}

// TestSessionlessRecordEmitsNothing: session_id maps to core's run_id and Emit
// rejects an empty one, so a record without it can only produce an event that
// fails later, further from the cause.
func TestSessionlessRecordEmitsNothing(t *testing.T) {
	rec := apiRequest(map[string]string{"session.id": ""})
	if _, ok := elected().EventFor(rec); ok {
		t.Error("emitted an event with no session id")
	}
}

// TestUnknownEventNamesAreIgnoredNotErrors: the export is a provider surface on
// a beta flag (OD3). An unrecognised event name must be a no-op, never a
// failure, or a routine upstream addition becomes a lane outage.
func TestUnknownEventNamesAreIgnoredNotErrors(t *testing.T) {
	rec := apiRequest(map[string]string{"event.name": "some_future_event"})
	rec.EventName = "some_future_event"
	if _, ok := elected().EventFor(rec); ok {
		t.Error("an unknown event name produced an event")
	}
}

// TestMalformedNumbersDoNotFabricateZeros: a token count that will not parse
// must be ABSENT, not zero. Zero is a measurement; absent is "unknown", and
// reporting a fabricated zero would silently understate spend.
func TestMalformedNumbersDoNotFabricateZeros(t *testing.T) {
	rec := apiRequest(map[string]string{"output_tokens": "not-a-number"})
	ev, ok := elected().EventFor(rec)
	if !ok {
		t.Fatal("no event")
	}
	if ev.Tokens.Output != nil {
		t.Errorf("output = %d, want absent — a fabricated zero understates spend", *ev.Tokens.Output)
	}
	if ev.Tokens.Input == nil || *ev.Tokens.Input != 2 {
		t.Error("one unparseable count discarded the others")
	}
	// Total must not silently omit the unknown part either.
	if ev.Tokens.Total != nil {
		t.Errorf("total = %d, want absent when a component is unknown", *ev.Tokens.Total)
	}
}

// TestDurationDerivesTheTurnWindow: the export gives a duration, not a start, so
// StartedAt is end - duration_ms. Without a window the span's start and end
// collapse and every latency reader shows zero.
func TestDurationDerivesTheTurnWindow(t *testing.T) {
	ev, ok := elected().EventFor(apiRequest(nil))
	if !ok {
		t.Fatal("no event")
	}
	start, err := time.Parse(time.RFC3339Nano, ev.StartedAt)
	if err != nil {
		t.Fatalf("started_at %q: %v", ev.StartedAt, err)
	}
	end, err := time.Parse(time.RFC3339Nano, ev.EndedAt)
	if err != nil {
		t.Fatalf("ended_at %q: %v", ev.EndedAt, err)
	}
	if got := end.Sub(start); got != 4210*time.Millisecond {
		t.Errorf("window = %v, want 4.21s", got)
	}
}

// TestEventIDIsDeterministic: INV-5. The spool can be drained by a different
// process long after the daemon that wrote it exited, so a redelivery has to
// present the same idempotency key or core stores a second row.
func TestEventIDIsDeterministic(t *testing.T) {
	a, _ := elected().EventFor(apiRequest(nil))
	b, _ := elected().EventFor(apiRequest(nil))
	if a.EventID == "" {
		t.Fatal("no event id (INV-5)")
	}
	if a.EventID != b.EventID {
		t.Errorf("event ids differ across identical records: %q vs %q", a.EventID, b.EventID)
	}
	c, _ := elected().EventFor(apiRequest(map[string]string{"request_id": "req_different"}))
	if c.EventID == a.EventID {
		t.Error("different calls share an idempotency key — core would drop one")
	}
}

// TestSessionIDIsValidatedLikeAPath is the defect this test was written for.
//
// session.id is a provider value off the same unauthenticated loopback listener
// as everything else, and it does not merely become the activity_id prefix and
// core's run_id — every spool consumer in this repo turns it into a FILENAME
// (`<session>.jsonl`, hookflow/spool.go). The mapper originally checked only
// non-empty, so a local process could have named the file.
//
// gatewayemit.usableSessionID already made this refusal; the asymmetry was the
// bug.
func TestSessionIDIsValidatedLikeAPath(t *testing.T) {
	for name, bad := range map[string]string{
		"parent traversal":  "../../etc/passwd",
		"a bare dot":        ".",
		"a bare double dot": "..",
		"a backslash":       `..\..\win`,
		"a colon":           "sess:1",
		"a newline":         "sess\n1",
		"empty":             "",
	} {
		t.Run(name, func(t *testing.T) {
			rec := apiRequest(map[string]string{"session.id": bad})
			if bad == "" {
				delete(rec.Attrs, "session.id")
			} else {
				rec.Attrs["session.id"] = bad
			}
			if ev, ok := elected().EventFor(rec); ok {
				t.Errorf("emitted an event whose session id is %q — it reaches a path join as %q.jsonl", bad, ev.SessionID)
			}
		})
	}
	// A real session id must still pass, or the guard is just an outage.
	rec := apiRequest(map[string]string{"session.id": "b3f1c2d4-0000-4000-8000-000000000001"})
	if _, ok := elected().EventFor(rec); !ok {
		t.Error("a UUID session id was rejected — all 59 in the corpus are UUIDs")
	}
}

// TestZeroTimestampIsDropped: record.go binds the record's own time and leaves a
// zero for "the mapper to decide what to do about". This is the decision.
//
// Formatting a zero time yields a VALID RFC3339 string in year 0001, so nothing
// downstream rejects it — the turn is simply filed a millennium out and every
// window and latency reader quietly disagrees with every other lane.
func TestZeroTimestampIsDropped(t *testing.T) {
	rec := apiRequest(nil)
	rec.Timestamp = time.Time{}
	if ev, ok := elected().EventFor(rec); ok {
		t.Errorf("emitted a turn stamped %q", ev.Timestamp)
	}
}
