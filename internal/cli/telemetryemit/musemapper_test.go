package telemetryemit

import (
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/sessionkey"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
)

const (
	museMain  = "feed0001-0000-4000-8000-000000000001"
	museChild = "feed0003-0000-4000-8000-000000000003"
)

// museModelCall is the shape of a Muse 1.4.1 `model_call` log record, scrubbed:
// attribute names exactly as exported (snake_case, no dots), metadata only.
func museModelCall(overrides map[string]string) telemetry.Record {
	attrs := map[string]string{
		"event_name":                     "model_call",
		"session_id":                     museMain,
		"session_root_id":                museMain,
		"session_kind":                   "main",
		"gen_ai_request_model":           "muse-spark-1.3-contributor",
		"gen_ai_provider_name":           "meta",
		"gen_ai_response_id":             "resp_scrubbed0002",
		"message_id":                     "feed0002-0000-4000-8000-000000000002",
		"gen_ai_usage_input_tokens":      "29513",
		"gen_ai_usage_output_tokens":     "119",
		"tokens_cached":                  "29297",
		"duration_ms":                    "4358",
		"time_to_first_event_ms":         "479",
		"gen_ai_response_finish_reasons": "[tool-calls]",
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
		EventName: attrs["event_name"],
		Timestamp: time.Date(2026, 9, 30, 18, 23, 55, 622e6, time.UTC),
		Attrs:     attrs,
	}
}

func museMapper() *Mapper {
	return elected().WithToolName("muse").WithFieldMap(MuseFieldMap).
		WithSessionAttr(sessionkey.OTelAttr(sessionkey.Muse))
}

func TestMuseModelCallBecomesOneLLMCompletionPair(t *testing.T) {
	events, out := museMapper().EventsFor(museModelCall(nil))
	if out != Emitted || len(events) != 2 {
		t.Fatalf("outcome %v, %d events; want Emitted and the Started/Completed pair", out, len(events))
	}
	started, completed := events[0], events[1]
	if started.EventType != client.EventTurnStarted || completed.EventType != client.EventTurnCompleted {
		t.Fatalf("types = %s, %s", started.EventType, completed.EventType)
	}
	for _, ev := range events {
		if ev.SessionID != museMain || ev.Tool.Name != "muse" || ev.ActivityType != client.ActivityTypeLLMCompletion {
			t.Errorf("%s: session=%q tool=%q activity=%q", ev.EventType, ev.SessionID, ev.Tool.Name, ev.ActivityType)
		}
		if ev.OtelRequestID != "resp_scrubbed0002" || ev.GatewayRequestID != "" || ev.ProxyRequestID != "" {
			t.Errorf("%s: request ids otel=%q gateway=%q proxy=%q", ev.EventType, ev.OtelRequestID, ev.GatewayRequestID, ev.ProxyRequestID)
		}
		if ev.Model != "muse-spark-1.3-contributor" {
			t.Errorf("%s: model = %q", ev.EventType, ev.Model)
		}
		if got := ev.Metadata["provider"]; got != "meta" {
			t.Errorf("%s: metadata provider = %v", ev.EventType, got)
		}
		if ev.Span == nil || ev.Span.SemanticType != client.ActivityTypeLLMCompletion {
			t.Errorf("%s: span = %+v", ev.EventType, ev.Span)
		}
	}
	// One namespace: the :otel: one, derived from the session and the response id.
	if id := client.WireActivityID(completed); id != museMain+":otel:resp_scrubbed0002" {
		t.Errorf("activity id = %q, want the :otel: namespace", id)
	}
	if client.WireActivityID(started) != client.WireActivityID(completed) {
		t.Error("the two halves do not share one activity id")
	}

	// Usage only on the close, through the allowlist. Muse's input count
	// INCLUDES the cached tokens (GenAI semconv 1.34; the capture shows an input
	// of 29513 with 29297 cached), so the total is input+output, not a sum that
	// counts the cache twice.
	if started.Tokens != nil {
		t.Error("the opening half carries usage")
	}
	tok := completed.Tokens
	if tok == nil || tok.Input == nil || tok.Output == nil || tok.CacheRead == nil || tok.Total == nil {
		t.Fatalf("tokens = %+v", tok)
	}
	if *tok.Input != 29513 || *tok.Output != 119 || *tok.CacheRead != 29297 || *tok.Total != 29513+119 {
		t.Errorf("tokens input=%d output=%d cache_read=%d total=%d", *tok.Input, *tok.Output, *tok.CacheRead, *tok.Total)
	}
	if tok.CacheCreationInput != nil {
		t.Error("Muse reports no cache writes; none may be invented")
	}

	start, _ := time.Parse(time.RFC3339Nano, started.StartedAt)
	end, _ := time.Parse(time.RFC3339Nano, completed.EndedAt)
	if end.Sub(start) != 4358*time.Millisecond {
		t.Errorf("window = %v, want duration_ms", end.Sub(start))
	}
}

func TestMuseRequestIDFallsBackToMessageID(t *testing.T) {
	for name, bad := range map[string]string{"absent": "", "a colon": "resp:1", "over the bound": strings.Repeat("r", 200)} {
		t.Run(name, func(t *testing.T) {
			ev, out := completedHalf(museMapper().EventsFor(museModelCall(map[string]string{"gen_ai_response_id": bad})))
			if out != Emitted || ev.OtelRequestID != "feed0002-0000-4000-8000-000000000002" {
				t.Fatalf("outcome %v, request id %q; want the message id", out, ev.OtelRequestID)
			}
		})
	}
	if _, out := museMapper().EventsFor(museModelCall(map[string]string{"gen_ai_response_id": "", "message_id": ""})); out != DropNoRequestID {
		t.Errorf("no id at all: outcome %v, want DropNoRequestID", out)
	}
}

func TestMuseEveryOtherEventIsSkipped(t *testing.T) {
	for _, name := range []string{"session_start", "session_end", "turn_start", "turn_end", "tool_call", "hook_run",
		"subagent_dispatch", "subagent_complete", "resource_pressure", "some_future_event", ""} {
		rec := museModelCall(map[string]string{"event_name": name})
		rec.EventName = name
		if events, out := museMapper().EventsFor(rec); out != SkipUnhandledEvent || len(events) != 0 {
			t.Errorf("%q: outcome %v with %d events, want SkipUnhandledEvent", name, out, len(events))
		}
	}
}

// TestMuseSubagentCallFoldsIntoTheSessionThatSpawnedIt: a subagent runs under a
// session id of its own and names the session at the top as session_root_id.
// The call is recorded in that session, tagged as a subagent's, the way the
// hook path folds the same child.
func TestMuseSubagentCallFoldsIntoTheSessionThatSpawnedIt(t *testing.T) {
	rec := museModelCall(map[string]string{"session_id": museChild, "session_kind": "reminder"})
	events, out := museMapper().EventsFor(rec)
	if out != Emitted {
		t.Fatalf("outcome %v", out)
	}
	for _, ev := range events {
		if ev.SessionID != museMain {
			t.Errorf("%s: session = %q, want the parent %q", ev.EventType, ev.SessionID, museMain)
		}
		if ev.Metadata["agent_type"] != "subagent" || ev.Metadata["agent_id"] != "reminder" {
			t.Errorf("%s: metadata = %v, want agent_type subagent and agent_id from session_kind", ev.EventType, ev.Metadata)
		}
	}

	// A main session (root equals id), and a record naming no root, are not folded.
	for name, o := range map[string]map[string]string{
		"main":          nil,
		"no root":       {"session_id": museChild, "session_root_id": ""},
		"unusable root": {"session_id": museChild, "session_root_id": "../../x"},
	} {
		ev, out := completedHalf(museMapper().EventsFor(museModelCall(o)))
		if out != Emitted {
			t.Fatalf("%s: outcome %v", name, out)
		}
		want := museMain
		if name != "main" {
			want = museChild
		}
		if ev.SessionID != want {
			t.Errorf("%s: session = %q, want %q", name, ev.SessionID, want)
		}
		if _, tagged := ev.Metadata["agent_type"]; tagged {
			t.Errorf("%s: an unfolded call is tagged as a subagent's: %v", name, ev.Metadata)
		}
	}
}

func TestMuseUnusableSessionIsDropped(t *testing.T) {
	for _, bad := range []string{"", "a:b", "..", "x/y"} {
		if _, out := museMapper().EventsFor(museModelCall(map[string]string{"session_id": bad, "session_root_id": ""})); out != DropBadSession {
			t.Errorf("session %q: outcome %v, want DropBadSession", bad, out)
		}
	}
}

func TestMuseUnelectedMapperEmitsNothing(t *testing.T) {
	m := New(testDID, Policy{}).WithToolName("muse").WithFieldMap(MuseFieldMap).
		WithSessionAttr(sessionkey.OTelAttr(sessionkey.Muse))
	if events, out := m.EventsFor(museModelCall(nil)); out != SkipNotElected || len(events) != 0 {
		t.Errorf("outcome %v with %d events", out, len(events))
	}
}

// TestMuseMapperDoesNotReadTheClaudeCodeKeys: a Muse-shaped mapper never reads
// `model`, `request_id` or `input_tokens`, so a Claude Code record handed to it
// by mistake emits nothing rather than a turn keyed on the wrong fields.
func TestMuseMapperDoesNotReadTheClaudeCodeKeys(t *testing.T) {
	if _, out := museMapper().EventsFor(apiRequest(nil)); out == Emitted {
		t.Error("a Claude Code api_request was mapped by the Muse field map")
	}
	// And the reverse: the default mapper ignores a Muse record.
	if _, out := elected().EventsFor(museModelCall(nil)); out == Emitted {
		t.Error("a Muse model_call was mapped by the default field map")
	}
}

// TestMuseContentNeverReachesTheWire: poisoned content-shaped attributes beside
// the real ones, through the real client at both capture postures.
func TestMuseContentNeverReachesTheWire(t *testing.T) {
	attrs := map[string]string{}
	for k, v := range sentinels {
		attrs[k] = v
	}
	attrs["gen_ai_input_messages"] = "SENTINEL_MESSAGES"
	attrs["gen_ai_tool_name"] = "SENTINEL_TOOLNAME"
	events, out := museMapper().EventsFor(museModelCall(attrs))
	if out != Emitted {
		t.Fatalf("outcome %v", out)
	}
	for _, captureOn := range []bool{false, true} {
		for _, ev := range events {
			body := emitThrough(t, captureOn, ev)
			for attr, marker := range attrs {
				if strings.HasPrefix(marker, "SENTINEL") && strings.Contains(body, marker) {
					t.Errorf("capture=%v: attribute %q leaked to the wire", captureOn, attr)
				}
			}
			if !strings.Contains(body, "resp_scrubbed0002") {
				t.Errorf("capture=%v: the accepted event does not carry its response id:\n%s", captureOn, body)
			}
		}
	}
}
