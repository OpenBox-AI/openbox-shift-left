package telemetryemit

import (
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
)

const testDID = "did:aip:openbox:dev:mapper-test"

// apiRequest builds the corpus's api_request shape.
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
	return New(testDID, Policy{Elected: func() bool { return true }})
}

// TestUnelectedMapperEmitsNothing is the single most important test in the
// file. A partially-built lane cannot double-count in the field, because the
// only policy anyone can construct today without saying "Elected" out loud is
// the one that emits nothing.
func TestUnelectedMapperEmitsNothing(t *testing.T) {
	m := New(testDID, Policy{})
	for _, name := range []string{"api_request", "api_response_body", "tool_result", "tool_decision", "user_prompt"} {
		rec := apiRequest(map[string]string{"event.name": name})
		rec.EventName = name
		if ev, out := completedHalf(m.EventsFor(rec)); out == Emitted {
			t.Errorf("%s: an UNELECTED mapper emitted %s; this doubles every token count on every dashboard", name, ev.EventType)
		}
	}
}

func TestAPIRequestBecomesTurnCompleted(t *testing.T) {
	ev, out := completedHalf(elected().EventsFor(apiRequest(nil)))
	if out != Emitted {
		t.Fatal("api_request produced no event")
	}

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
		t.Errorf("model = %q; this is core's aggregation key", ev.Model)
	}

	if ev.OtelRequestID != "req_011CeSoFqW2HfEh9jxCds86Y" {
		t.Errorf("otel_request_id = %q, want the provider's request_id", ev.OtelRequestID)
	}
	if ev.GatewayRequestID != "" || ev.ProxyRequestID != "" {
		t.Error("an otel event set another lane's discriminator; the contract's turnProducer oneOf requires exactly one")
	}

	if ev.Tokens == nil {
		t.Fatal("no tokens; the whole point of this lane")
	}
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

	if ev.Span == nil {
		t.Fatal("no span; core cannot classify this as llm_completion without one")
	}
	if ev.Span.SemanticType != "llm_completion" {
		t.Errorf("span semantic_type = %q", ev.Span.SemanticType)
	}
}

// TestOtelRequestIDRejectsMalformedProviderValues guards event identity.
// OtelRequestID becomes part of activity_id, which is this product's event
// identity and is byte-pinned and load-bearing for core's dedupe.
func TestOtelRequestIDRejectsMalformedProviderValues(t *testing.T) {
	cases := map[string]string{
		"a colon breaks the namespace": "req:with:colons",
		"whitespace":                   "req with spaces",
		"a newline":                    "req\nid",
		"over the length bound":        strings.Repeat("a", 200),
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			rec := apiRequest(map[string]string{"request_id": bad, "client_request_id": bad})
			if ev, out := completedHalf(elected().EventsFor(rec)); out == Emitted {
				t.Errorf("emitted an event with otel_request_id %q (activity_id would be %q…)", ev.OtelRequestID, ev.SessionID+":otel:"+ev.OtelRequestID)
			}
		})
	}
}

func TestOtelRequestIDFallsBackToClientRequestID(t *testing.T) {
	rec := apiRequest(map[string]string{"request_id": ""})
	ev, out := completedHalf(elected().EventsFor(rec))
	if out != Emitted {
		t.Fatal("no event; client_request_id should have served as the id")
	}
	if ev.OtelRequestID != "b3f1c2d4-0000-4000-8000-000000000001" {
		t.Errorf("otel_request_id = %q, want the client_request_id", ev.OtelRequestID)
	}
}

func TestNoIDAtAllEmitsNothing(t *testing.T) {
	rec := apiRequest(map[string]string{"request_id": "", "client_request_id": ""})
	if _, out := completedHalf(elected().EventsFor(rec)); out == Emitted {
		t.Error("emitted a turn with no provider id; a minted id would break idempotency across a re-flush")
	}
}

// TestSessionlessRecordEmitsNothing: session_id maps to core's run_id and Emit
// rejects an empty one, so a record without it can only produce an event that
// fails later, further from the cause.
func TestSessionlessRecordEmitsNothing(t *testing.T) {
	rec := apiRequest(map[string]string{"session.id": ""})
	if _, out := completedHalf(elected().EventsFor(rec)); out == Emitted {
		t.Error("emitted an event with no session id")
	}
}

// TestUnknownEventNamesAreIgnoredNotErrors: the export is a provider surface
// on a beta flag. An unrecognised event name must be a no-op, never a
// failure, or a routine upstream addition becomes a lane outage.
func TestUnknownEventNamesAreIgnoredNotErrors(t *testing.T) {
	rec := apiRequest(map[string]string{"event.name": "some_future_event"})
	rec.EventName = "some_future_event"
	if _, out := completedHalf(elected().EventsFor(rec)); out == Emitted {
		t.Error("an unknown event name produced an event")
	}
}

// TestMalformedNumbersDoNotFabricateZeros: a token count that will not parse
// must be absent, not zero. Zero is a measurement; absent is "unknown", and
// reporting a fabricated zero would silently understate spend.
func TestMalformedNumbersDoNotFabricateZeros(t *testing.T) {
	rec := apiRequest(map[string]string{"output_tokens": "not-a-number"})
	ev, out := completedHalf(elected().EventsFor(rec))
	if out != Emitted {
		t.Fatal("no event")
	}
	if ev.Tokens.Output != nil {
		t.Errorf("output = %d, want absent; a fabricated zero understates spend", *ev.Tokens.Output)
	}
	if ev.Tokens.Input == nil || *ev.Tokens.Input != 2 {
		t.Error("one unparseable count discarded the others")
	}
	if ev.Tokens.Total != nil {
		t.Errorf("total = %d, want absent when a component is unknown", *ev.Tokens.Total)
	}
}

// TestDurationDerivesTheTurnWindow: the export gives a duration, not a start,
// so StartedAt is end - duration_ms.
func TestDurationDerivesTheTurnWindow(t *testing.T) {
	ev, out := completedHalf(elected().EventsFor(apiRequest(nil)))
	if out != Emitted {
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

// TestEventIDIsDeterministic: a retry must present the same idempotency key.
func TestEventIDIsDeterministic(t *testing.T) {
	a, _ := completedHalf(elected().EventsFor(apiRequest(nil)))
	b, _ := completedHalf(elected().EventsFor(apiRequest(nil)))
	if a.EventID == "" {
		t.Fatal("no event id")
	}
	if a.EventID != b.EventID {
		t.Errorf("event ids differ across identical records: %q vs %q", a.EventID, b.EventID)
	}
	c, _ := completedHalf(elected().EventsFor(apiRequest(map[string]string{"request_id": "req_different"})))
	if c.EventID == a.EventID {
		t.Error("different calls share an idempotency key; core would drop one")
	}
}

// TestSessionIDIsValidatedLikeAPath is the defect this test was written for.
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
			if ev, out := completedHalf(elected().EventsFor(rec)); out == Emitted {
				t.Errorf("emitted an event whose session id is %q; it reaches a path join as %q.jsonl", bad, ev.SessionID)
			}
		})
	}
	rec := apiRequest(map[string]string{"session.id": "b3f1c2d4-0000-4000-8000-000000000001"})
	if _, out := completedHalf(elected().EventsFor(rec)); out != Emitted {
		t.Error("a UUID session id was rejected; all 59 in the corpus are UUIDs")
	}
}

// TestZeroTimestampIsDropped: record.go binds the record's own time and leaves
// a zero for "the mapper to decide what to do about".
func TestZeroTimestampIsDropped(t *testing.T) {
	rec := apiRequest(nil)
	rec.Timestamp = time.Time{}
	if ev, out := completedHalf(elected().EventsFor(rec)); out == Emitted {
		t.Errorf("emitted a turn stamped %q", ev.Timestamp)
	}
}

// TestOutcomeSeparatesSkipsFromDrops keeps skips and drops apart. A lane
// that goes quiet because every record now fails validation must not look
// identical to a quiet session.
func TestOutcomeSeparatesSkipsFromDrops(t *testing.T) {
	cases := []struct {
		name     string
		rec      telemetry.Record
		mapper   *Mapper
		want     Outcome
		wantDrop bool
	}{
		{"unelected is a skip, not a drop", apiRequest(nil), New(testDID, Policy{}), SkipNotElected, false},
		{"an unbound event type is a skip", func() telemetry.Record {
			r := apiRequest(nil)
			r.EventName = "retention_sweep"
			return r
		}(), elected(), SkipUnhandledEvent, false},
		{"an unusable session is a DROP", func() telemetry.Record {
			r := apiRequest(nil)
			r.Attrs["session.id"] = "../escape"
			return r
		}(), elected(), DropBadSession, true},
		{"no request id is a DROP", func() telemetry.Record {
			r := apiRequest(nil)
			delete(r.Attrs, "request_id")
			delete(r.Attrs, "client_request_id")
			return r
		}(), elected(), DropNoRequestID, true},
		{"a zero timestamp is a DROP", func() telemetry.Record {
			r := apiRequest(nil)
			r.Timestamp = time.Time{}
			return r
		}(), elected(), DropNoTimestamp, true},
		{"a good record is emitted", apiRequest(nil), elected(), Emitted, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, out := completedHalf(c.mapper.EventsFor(c.rec))
			if out != c.want {
				t.Errorf("outcome = %v (%s), want %v (%s)", int(out), out, int(c.want), c.want)
			}
			if out.IsDrop() != c.wantDrop {
				t.Errorf("IsDrop() = %v, want %v; the daemon warns on drops and stays quiet on skips, so this classification decides whether a broken lane is noticed", out.IsDrop(), c.wantDrop)
			}
			if out.String() == "unknown" {
				t.Errorf("outcome %d has no name; it reaches operator-facing output", int(out))
			}
		})
	}
}

// TestElectionIsAnsweredPerRecordNotAtConstruction: an election answered once
// at construction lets a lane emit turns it has already lost the right to emit.
func TestElectionIsAnsweredPerRecordNotAtConstruction(t *testing.T) {
	elected := true
	m := New(testDID, Policy{Elected: func() bool { return elected }})

	if _, outcome := completedHalf(m.EventsFor(apiRequest(nil))); outcome == SkipNotElected {
		t.Fatalf("the mapper refused a record while elected: %v", outcome)
	}

	elected = false
	if _, outcome := completedHalf(m.EventsFor(apiRequest(nil))); outcome != SkipNotElected {
		t.Errorf("outcome = %v after losing the election; the lane kept emitting, "+
			"so two producers now describe the same model call and every token count doubles", outcome)
	}

	elected = true
	if _, outcome := completedHalf(m.EventsFor(apiRequest(nil))); outcome == SkipNotElected {
		t.Error("the lane stayed silent after regaining the election; only a daemon restart would fix it")
	}
}

// TestAnUnsetElectionGateSuppresses keeps the zero value's guarantee
// structural now that it is a function: a half-built caller that never names
// Elected must emit nothing, exactly as it did when this was a bool.
func TestAnUnsetElectionGateSuppresses(t *testing.T) {
	if _, outcome := New(testDID, Policy{}).EventsFor(apiRequest(nil)); outcome != SkipNotElected {
		t.Errorf("outcome = %v for a policy that never named a gate; the zero value must suppress", outcome)
	}
}

// TestAttributionMetadataBindsBothKeysOnBothHalves proves query_source and
// prompt_id land in metadata, verbatim, on BOTH the Started and Completed
// half -- not just the completed half most tests in this file exercise.
func TestAttributionMetadataBindsBothKeysOnBothHalves(t *testing.T) {
	rec := apiRequest(map[string]string{
		"query_source": "prompt_suggestion",
		"prompt.id":    "b3f1c2d4-1111-4000-8000-000000000099",
	})
	events, out := elected().EventsFor(rec)
	if out != Emitted {
		t.Fatal("no events")
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (Started + Completed)", len(events))
	}
	for _, ev := range events {
		if got := ev.Metadata["query_source"]; got != "prompt_suggestion" {
			t.Errorf("%s: metadata.query_source = %v, want prompt_suggestion", ev.EventType, got)
		}
		if got := ev.Metadata["prompt_id"]; got != "b3f1c2d4-1111-4000-8000-000000000099" {
			t.Errorf("%s: metadata.prompt_id = %v, want the verbatim prompt.id attribute", ev.EventType, got)
		}
	}
}

// TestUnknownQuerySourceSurvivesVerbatim is the anti-enumOr guard: a
// query_source value never seen in the corpus or the provider binary must
// still reach metadata unchanged, because that vocabulary is vendor-owned
// and this lane does not allowlist it.
func TestUnknownQuerySourceSurvivesVerbatim(t *testing.T) {
	rec := apiRequest(map[string]string{"query_source": "some_future_source"})
	ev, out := completedHalf(elected().EventsFor(rec))
	if out != Emitted {
		t.Fatal("no event")
	}
	if got := ev.Metadata["query_source"]; got != "some_future_source" {
		t.Errorf("metadata.query_source = %v, want the unrecognized value verbatim (no allowlist)", got)
	}
}

// TestAbsentAttributionAttrsYieldNilMetadata: neither attr present must leave
// Metadata nil, not {"query_source":""}. An empty-string value would be
// indistinguishable from a disallowed default.
func TestAbsentAttributionAttrsYieldNilMetadata(t *testing.T) {
	events, out := elected().EventsFor(apiRequest(nil))
	if out != Emitted {
		t.Fatal("no events")
	}
	for _, ev := range events {
		if ev.Metadata != nil {
			t.Errorf("%s: metadata = %v, want nil when query_source and prompt.id are both absent", ev.EventType, ev.Metadata)
		}
	}
}

// TestPromptIDAloneBindsWithoutQuerySource: the two keys are independent
// binds. Either may be present without the other, and the absent one must
// not receive a substituted default.
func TestPromptIDAloneBindsWithoutQuerySource(t *testing.T) {
	rec := apiRequest(map[string]string{"prompt.id": "only-prompt-id"})
	ev, out := completedHalf(elected().EventsFor(rec))
	if out != Emitted {
		t.Fatal("no event")
	}
	if _, present := ev.Metadata["query_source"]; present {
		t.Errorf("metadata.query_source present (%v), want absent: no default may be substituted", ev.Metadata["query_source"])
	}
	if got := ev.Metadata["prompt_id"]; got != "only-prompt-id" {
		t.Errorf("metadata.prompt_id = %v, want only-prompt-id", got)
	}
}

// TestAttributionMetadataCapsAHostileQuerySource proves the bind itself
// applies the identifier bound, not just the helper in isolation: a 300 KB
// query_source attribute must not reach metadata uncapped.
func TestAttributionMetadataCapsAHostileQuerySource(t *testing.T) {
	hostile := strings.Repeat("a", 300000)
	rec := apiRequest(map[string]string{"query_source": hostile})
	ev, out := completedHalf(elected().EventsFor(rec))
	if out != Emitted {
		t.Fatal("no event")
	}
	got, _ := ev.Metadata["query_source"].(string)
	if len(got) == 0 {
		t.Fatal("metadata.query_source was capped to nothing; the bound should truncate, not erase")
	}
	if len(got) > 256 {
		t.Errorf("metadata.query_source is %d bytes, want <= 256", len(got))
	}
}

// TestToolNameDefaultsToClaudeCode pins the byte-identical fixture: a mapper
// that never heard of WithToolName keeps shipping "claude-code" on both
// halves.
func TestToolNameDefaultsToClaudeCode(t *testing.T) {
	m := elected()
	for i, ev := range mustPair(t, m, apiRequest(nil)) {
		if ev.Tool.Name != "claude-code" {
			t.Errorf("half %d: Tool.Name = %q, want the default %q", i, ev.Tool.Name, "claude-code")
		}
	}
}

// TestWithToolNameStampsBothHalves: a pre-resolved per-provider mapper can
// carry its own tool name, on both halves of the pair.
func TestWithToolNameStampsBothHalves(t *testing.T) {
	m := elected().WithToolName("codex")
	for i, ev := range mustPair(t, m, apiRequest(nil)) {
		if ev.Tool.Name != "codex" {
			t.Errorf("half %d: Tool.Name = %q, want %q", i, ev.Tool.Name, "codex")
		}
	}
}

// TestSessionAttrDefaultsToSessionID pins the CC arm byte-identical: a mapper
// that never heard of WithSessionAttr reads only session.id, exactly as
// before this field existed -- a record carrying conversation.id instead
// must still DROP, never silently key on the other attribute.
func TestSessionAttrDefaultsToSessionID(t *testing.T) {
	m := elected()
	rec := apiRequest(map[string]string{"session.id": ""})
	rec.Attrs["conversation.id"] = "conv-1"
	if _, out := completedHalf(m.EventsFor(rec)); out != DropBadSession {
		t.Fatalf("outcome = %v, want DropBadSession; the default mapper must not read conversation.id", out)
	}
}

// TestWithSessionAttrKeysOnConversationID is Codex's minimal attributability:
// a mapper opted into the conversation.id surface keys a turn on it when
// session.id is the one CC would have used, but a Codex mapper is never
// given that key. It emits, and Tool.Name still reflects WithToolName.
func TestWithSessionAttrKeysOnConversationID(t *testing.T) {
	m := elected().WithSessionAttr("conversation.id").WithToolName("codex")
	rec := apiRequest(map[string]string{"session.id": ""})
	rec.Attrs["conversation.id"] = "conv-1"
	ev, out := completedHalf(m.EventsFor(rec))
	if out != Emitted {
		t.Fatalf("outcome = %v, want Emitted", out)
	}
	if ev.SessionID != "conv-1" {
		t.Errorf("SessionID = %q, want the conversation.id value %q", ev.SessionID, "conv-1")
	}
	if ev.Tool.Name != "codex" {
		t.Errorf("Tool.Name = %q, want %q", ev.Tool.Name, "codex")
	}
}

// mustPair is EventsFor for a test that wants both halves and expects them to
// be produced; it fails loudly rather than silently trimming to zero events.
func mustPair(t *testing.T, m *Mapper, rec telemetry.Record) []client.DevEvent {
	t.Helper()
	events, out := m.EventsFor(rec)
	if out != Emitted || len(events) != 2 {
		t.Fatalf("EventsFor: outcome=%v events=%d, want Emitted and the pair", out, len(events))
	}
	return events
}

// completedHalf adapts the pair to a test that means "the event": the closing
// half, which is the one carrying the usage and the duration.
func completedHalf(events []client.DevEvent, outcome Outcome) (client.DevEvent, Outcome) {
	if len(events) == 0 {
		return client.DevEvent{}, outcome
	}
	return events[len(events)-1], outcome
}
