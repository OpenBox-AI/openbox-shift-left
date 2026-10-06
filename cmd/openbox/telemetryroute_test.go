package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/sessionkey"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/telemetryemit"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
)

const routeDID = "did:aip:7f3c9b2e-0000-5000-a000-00000000feed"

// routedEmitters builds one elected emitter per tool over a shared capture, the
// way the daemon wires them, so a record's destination is the Tool.Name of
// what was delivered.
func routedEmitters(delivered *deliveredEvents, tools ...provider.Name) map[string]*telemetryemit.Emitter {
	out := map[string]*telemetryemit.Emitter{}
	for _, tool := range tools {
		m := telemetryemit.New(routeDID, telemetryemit.Policy{Elected: func() bool { return true }})
		switch tool {
		case provider.Codex:
			m = m.WithSessionAttr(sessionkey.OTelAttr(sessionkey.Codex)).WithToolName(string(provider.Codex)).
				WithFieldMap(telemetryemit.CodexFieldMap)
		case provider.Muse:
			m = m.WithSessionAttr(sessionkey.OTelAttr(sessionkey.Muse)).WithToolName(string(provider.Muse)).
				WithFieldMap(telemetryemit.MuseFieldMap)
		}
		out[string(tool)] = &telemetryemit.Emitter{Mapper: m, DID: func() string { return routeDID }, Warn: func(string, ...any) {}, Deliver: delivered.Deliver}
	}
	return out
}

func routeRecord(event string, attrs map[string]string) telemetry.Record {
	return telemetry.Record{Signal: telemetry.SignalLogs, EventName: event,
		Timestamp: time.Date(2026, 9, 30, 18, 0, 0, 0, time.UTC), Attrs: attrs}
}

var (
	ccRecord = routeRecord("api_request", map[string]string{
		"session.id": "cc-1", "request_id": "req-cc", "model": "claude-opus-4-8", "input_tokens": "1"})
	codexRecord = routeRecord("codex.sse_event", map[string]string{
		"event.kind": "response.completed", "conversation.id": "thread-1", "model": "gpt-5-codex", "input_token_count": "1"})
	museRecord = routeRecord("model_call", map[string]string{
		"session_id": "muse-1", "gen_ai_response_id": "resp-muse", "gen_ai_request_model": "muse-spark",
		"gen_ai_usage_input_tokens": "1"})
)

// TestTelemetryRoutingTable: the per-provider table routes on which session
// attribute a record carries, refuses any record that carries more than one
// tool's, and sends an unkeyed record to the Claude Code emitter so its drop is
// counted rather than lost.
func TestTelemetryRoutingTable(t *testing.T) {
	both := func(a, b telemetry.Record) telemetry.Record {
		r := routeRecord(a.EventName, map[string]string{})
		for k, v := range a.Attrs {
			r.Attrs[k] = v
		}
		for k, v := range b.Attrs {
			r.Attrs[k] = v
		}
		return r
	}
	for _, tc := range []struct {
		name      string
		rec       telemetry.Record
		wantTool  string // Tool.Name of the delivered pair; "" = nothing delivered
		wantAmbig bool
	}{
		{"claude code", ccRecord, "claude-code", false},
		{"codex", codexRecord, "codex", false},
		{"muse", museRecord, "muse", false},
		{"cc and codex", both(ccRecord, codexRecord), "", true},
		{"cc and muse", both(ccRecord, museRecord), "", true},
		{"codex and muse", both(codexRecord, museRecord), "", true},
		{"all three", both(both(ccRecord, codexRecord), museRecord), "", true},
		{"no session key at all", routeRecord("api_request", map[string]string{"request_id": "r"}), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delivered := &deliveredEvents{}
			em := newTelemetryRouter(routedEmitters(delivered, provider.ClaudeCode, provider.Codex, provider.Muse))
			err := em.Emit(context.Background(), tc.rec)
			if tc.wantAmbig != errors.Is(err, errAmbiguousTelemetryProvider) {
				t.Fatalf("Emit = %v, want ambiguity %v", err, tc.wantAmbig)
			}
			events := delivered.asMaps(t)
			if tc.wantTool == "" {
				if len(events) != 0 {
					t.Fatalf("delivered %d events, want none", len(events))
				}
				return
			}
			if len(events) != 2 {
				t.Fatalf("delivered %d events, want the pair", len(events))
			}
			if got := events[0]["tool"].(map[string]any)["name"]; got != tc.wantTool {
				t.Errorf("tool = %v, want %s", got, tc.wantTool)
			}
		})
	}
}

// TestTelemetryRoutingWithoutAMuseIdentityStillCountsTheDrop: a Muse-shaped
// record on a machine with no Muse emitter reaches the Claude Code emitter's
// Emit, which drops it and counts it, rather than vanishing.
func TestTelemetryRoutingWithoutAMuseIdentityStillCountsTheDrop(t *testing.T) {
	delivered := &deliveredEvents{}
	emitters := routedEmitters(delivered, provider.ClaudeCode)
	em := newTelemetryRouter(emitters)
	if err := em.Emit(context.Background(), museRecord); err != nil {
		t.Fatal(err)
	}
	if n := len(delivered.asMaps(t)); n != 0 {
		t.Fatalf("delivered %d events for an unrouted Muse record", n)
	}
	if _, drops := emitters[string(provider.ClaudeCode)].Stats(); len(drops) == 0 {
		t.Error("the Muse-shaped record was dropped without being counted")
	}
}
