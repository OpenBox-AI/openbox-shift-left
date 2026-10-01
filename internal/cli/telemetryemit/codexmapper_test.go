package telemetryemit

import (
	"context"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/sessionkey"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
)

const codexThread = "01a0f6b9-501d-7840-b9d2-9ac73d20b213"

// codexSSE is the shape of Codex 0.156.1's `codex.sse_event` log record:
// attribute names exactly as exported, keys read off a live capture. There is
// no request id, no duration and no response id on it.
func codexSSE(overrides map[string]string) telemetry.Record {
	attrs := map[string]string{
		"event.name":              "codex.sse_event",
		"event.kind":              "response.completed",
		"event.timestamp":         "2026-10-01T09:09:00.900Z",
		"conversation.id":         codexThread,
		"model":                   "gpt-5.3-codex",
		"input_token_count":       "16225",
		"output_token_count":      "117",
		"cached_token_count":      "7680",
		"cache_write_token_count": "0",
		"reasoning_token_count":   "0",
		"tool_token_count":        "0",
		"originator":              "codex_cli_rs",
		"user.email":              "dev@example.invalid",
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
		Timestamp: time.Date(2026, 10, 1, 9, 9, 0, 900e6, time.UTC),
		Attrs:     attrs,
	}
}

func codexMapper() *Mapper {
	return elected().WithToolName("codex").WithFieldMap(CodexFieldMap).
		WithSessionAttr(sessionkey.OTelAttr(sessionkey.Codex))
}

func TestCodexSSECompletedBecomesOnePairWithTokens(t *testing.T) {
	events, out := codexMapper().EventsFor(codexSSE(nil))
	if out != Emitted || len(events) != 2 {
		t.Fatalf("outcome %v, %d events; want Emitted and the pair", out, len(events))
	}
	if events[0].EventType != client.EventTurnStarted || events[1].EventType != client.EventTurnCompleted {
		t.Fatalf("pair order %v, %v", events[0].EventType, events[1].EventType)
	}
	c := events[1]
	if c.SessionID != codexThread || c.Model != "gpt-5.3-codex" || c.Tool.Name != "codex" {
		t.Errorf("session/model/tool = %q/%q/%q", c.SessionID, c.Model, c.Tool.Name)
	}
	if c.OtelRequestID == "" || c.OtelRequestID != events[0].OtelRequestID {
		t.Errorf("request id %q / %q: want one minted id on both halves", events[0].OtelRequestID, c.OtelRequestID)
	}
	tk := c.Tokens
	if tk == nil || tk.Input == nil || *tk.Input != 16225 || *tk.Output != 117 || *tk.CacheRead != 7680 {
		t.Fatalf("tokens = %+v", tk)
	}
	// The cached count is inside the input count, so the total is input+output.
	if tk.Total == nil || *tk.Total != 16225+117 {
		t.Errorf("total = %v, want %d (cache is inside input)", tk.Total, 16225+117)
	}
}

func TestCodexOnlyResponseCompletedIsAModelCall(t *testing.T) {
	for _, kind := range []string{"response.output_text.delta", "response.created", "response.failed", ""} {
		if _, out := codexMapper().EventsFor(codexSSE(map[string]string{"event.kind": kind})); out != SkipUnhandledEvent {
			t.Errorf("event.kind %q: outcome %v, want SkipUnhandledEvent", kind, out)
		}
	}
	// The pre-0.156 / Claude Code name is not Codex's record.
	if _, out := codexMapper().EventsFor(codexSSE(map[string]string{"event.name": "codex.api_request"})); out != SkipUnhandledEvent {
		t.Errorf("codex.api_request: outcome %v, want SkipUnhandledEvent", out)
	}
}

// The minted id is a pure function of the record: a restart must reproduce
// it (so core dedupes a redelivery), and two calls of one thread must not
// share one (or core absorbs the second as a duplicate of the first).
func TestCodexMintedRequestIDIsStableAndDistinctPerCall(t *testing.T) {
	a1, _ := codexMapper().EventsFor(codexSSE(nil))
	a2, _ := codexMapper().EventsFor(codexSSE(nil))
	if a1[0].OtelRequestID != a2[0].OtelRequestID || a1[0].EventID != a2[0].EventID {
		t.Fatalf("the same record minted %q then %q", a1[0].OtelRequestID, a2[0].OtelRequestID)
	}
	later := codexSSE(nil)
	later.Timestamp = later.Timestamp.Add(2 * time.Second)
	later.Attrs["event.timestamp"] = "2026-10-01T09:09:02.900Z"
	b, _ := codexMapper().EventsFor(later)
	if b[0].OtelRequestID == a1[0].OtelRequestID {
		t.Error("two calls of one thread with identical usage share a request id")
	}
	other, _ := codexMapper().EventsFor(codexSSE(map[string]string{"conversation.id": "01a0f6ba-0c89-7201-9966-11613fa3d74c"}))
	if other[0].OtelRequestID == a1[0].OtelRequestID {
		t.Error("two threads share a request id")
	}
	if !safeRequestID(a1[0].OtelRequestID) {
		t.Errorf("minted id %q is not a safe activity id component", a1[0].OtelRequestID)
	}
}

func TestCodexRecordWithoutThreadOrTimeIsDropped(t *testing.T) {
	if _, out := codexMapper().EventsFor(codexSSE(map[string]string{"conversation.id": ""})); out != DropBadSession {
		t.Errorf("no conversation.id: %v", out)
	}
	rec := codexSSE(nil)
	rec.Timestamp = time.Time{}
	if _, out := codexMapper().EventsFor(rec); out != DropNoTimestamp {
		t.Errorf("no timestamp: %v", out)
	}
}

// The default (Claude Code) field map still reads api_request and still does
// not know Codex's record.
func TestDefaultFieldMapIgnoresCodexSSE(t *testing.T) {
	if _, out := elected().WithSessionAttr(sessionkey.OTelAttr(sessionkey.Codex)).EventsFor(codexSSE(nil)); out != SkipUnhandledEvent {
		t.Errorf("default field map on codex.sse_event: %v", out)
	}
}

// The seam hands the enricher what a rollout join needs and nothing from the
// attributes: the record's time and the pair's token counts.
func TestEnrichCallCarriesTimeAndTokens(t *testing.T) {
	got := make(chan Call, 1)
	s := newSink()
	e := &Emitter{
		Mapper:  codexMapper(),
		DID:     func() string { return testDID },
		Deliver: s.deliver,
		Enrich: &Enricher{Outcome: "codex.content", Enrich: func(_ context.Context, c Call) Result {
			got <- c
			return Result{}
		}},
	}
	rec := codexSSE(nil)
	if err := e.Emit(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	s.waitFor(t, 2)
	e.Wait()
	c := <-got
	if c.Session != codexThread || c.RequestID == "" {
		t.Errorf("call = %+v", c)
	}
	if !c.At.Equal(rec.Timestamp) {
		t.Errorf("At = %v, want the record time %v", c.At, rec.Timestamp)
	}
	if c.Tokens == nil || c.Tokens.Input == nil || *c.Tokens.Input != 16225 || *c.Tokens.Output != 117 {
		t.Errorf("tokens = %+v", c.Tokens)
	}
}
