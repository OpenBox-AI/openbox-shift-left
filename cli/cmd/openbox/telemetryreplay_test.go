package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/telemetryemit"
	"github.com/openbox-ai/openbox-shift-left/telemetry"
)

// telemetryreplay_test.go — the recorded-traffic replay for the :otel: lane.
//
// TestTelemetryCommandActuallyRecords next door is the SOCKET control: it drives
// the real command over a real port and is skipped on a host that cannot bind.
// This is the bind-free twin, and it trades exactly one thing for coverage: the
// OTLP HTTP layer. Everything from the collector's own decode onward is the
// shipped chain — the production projection, the production mapper, the
// production emitter, a real spool file read back off disk. No fake anywhere.
//
// The entry point is the rule, and it is what separates this from a test that
// looks the same and proves much less. The fixture is OTLP wire JSON recorded
// from a real desktop session; it is decoded by the collector's unmarshaler
// through Receiver.ConsumeLogsJSON. A version of this test that constructed
// telemetry.Record values and handed them to the mapper would assert arithmetic
// on our own struct while claiming to prove a mapping of real provider traffic.

// replayCorpus reads the committed fixture from the telemetry module.
//
// It FAILS rather than skips when the file is missing. A replay suite that
// quietly covers nothing is indistinguishable from one that works, which is the
// failure this phase exists to remove.
func replayCorpus(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "..", "telemetry", "testdata", "corpus", "otel-logs.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recorded corpus fixture %s: %v", path, err)
	}
	return raw
}

// replayThroughChain runs the fixture through the shipped chain and returns every
// event that reached the spool, plus the emitter's own outcome counters.
func replayThroughChain(t *testing.T, elected bool) ([]map[string]any, int, map[string]int) {
	t.Helper()
	spoolDir := t.TempDir()

	em := &telemetryemit.Emitter{
		Spool: hookflow.Spool{Dir: spoolDir},
		Mapper: telemetryemit.New("did:aip:7f3c9b2e-0000-5000-a000-00000000feed",
			telemetryemit.Policy{Elected: func() bool { return elected }}),
		DID:  func() string { return "did:aip:7f3c9b2e-0000-5000-a000-00000000feed" },
		Warn: func(string, ...any) {},
		// No Flush: the detached realtime flusher must never be spawned from a
		// test binary, and delivery is not what this asserts.
	}

	rec, err := telemetry.New(telemetry.Config{Addr: "127.0.0.1:0"}, telemetry.WithEmitter(em))
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	if err := rec.ConsumeLogsJSON(context.Background(), replayCorpus(t)); err != nil {
		t.Fatalf("replaying the recorded export: %v", err)
	}

	emitted, drops := em.Stats()
	return readAllSpooled(t, spoolDir), emitted, drops
}

// readAllSpooled reads every spooled DevEvent, in file order.
//
// Field names here are the STRUCT's, not the wire's: the spool holds the pre-wire
// DevEvent and buildPayload mints session_id/activity_id on the way out. Reading
// the struct with the wire's vocabulary is what once made a working lane report
// an empty spool.
func readAllSpooled(t *testing.T, spoolDir string) []map[string]any {
	t.Helper()
	var out []map[string]any
	err := filepath.WalkDir(spoolDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			var ev map[string]any
			if json.Unmarshal([]byte(line), &ev) == nil {
				out = append(out, ev)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the spool: %v", err)
	}
	return out
}

// TestTelemetryReplayMapsRecordedTrafficToTurns is the positive half.
func TestTelemetryReplayMapsRecordedTrafficToTurns(t *testing.T) {
	events, emitted, drops := replayThroughChain(t, true)

	if len(events) == 0 {
		t.Fatal("the recorded export produced no spooled event; the chain is broken somewhere between the collector's decode and the spool")
	}
	if emitted != len(events) {
		t.Errorf("emitter counted %d emissions but %d events reached the spool", emitted, len(events))
	}

	// The census fixture carries five api_request records and fifteen other
	// event types. Exactly the five become turns.
	if len(events) != 5 {
		t.Errorf("got %d turns from the recorded export, want 5 (one per recorded api_request)", len(events))
	}

	// A DROP MUST BE COUNTABLE. Phase 09 inherited that pin for a reason: a lane
	// failing validation on every record looks identical to a quiet session.
	// Fifteen records are unhandled event types, so the counter must say so.
	if drops["unhandled-event"] == 0 {
		t.Errorf("no unhandled-event drops were counted; a lane that drops silently cannot be told from a quiet session. counters: %v", drops)
	}

	ids := map[string]bool{}
	for _, ev := range events {
		if got := ev["event_type"]; got != "TurnCompleted" {
			t.Errorf("event_type = %v, want TurnCompleted", got)
		}
		reqID, _ := ev["otel_request_id"].(string)
		if reqID == "" {
			t.Error("otel_request_id is empty; without the lane discriminator turnActivityIDFor returns an EMPTY activity_id")
		}
		if ids[reqID] {
			t.Errorf("two turns share otel_request_id %q; core's dedupe would absorb one and half the evidence would vanish", reqID)
		}
		ids[reqID] = true

		if model, _ := ev["model"].(string); model == "" {
			t.Error("no model on the turn; it is core's aggregation key")
		}
		tokens, _ := ev["tokens"].(map[string]any)
		if tokens == nil {
			t.Errorf("no tokens on the turn, which is this lane's whole payload: %v", ev)
		}
		span, _ := ev["span"].(map[string]any)
		if span == nil {
			t.Error("no span; core recomputes semantic_type per span and this is the only path to llm_completion")
			continue
		}
		if got := span["semantic_type"]; got != "llm_completion" {
			t.Errorf("span.semantic_type = %v, want llm_completion", got)
		}
	}
}

// TestTelemetryReplayCarriesNoRecordedContent is the privacy half, asserted on
// what actually reached the spool rather than on what the mapper intended.
//
// The recorded export carries prompts, assistant responses and tool output on
// its own attributes — api_request_body and api_response_body are two of the
// sixteen event types in the fixture. The projection binds identifiers and
// numbers; nothing in this lane attaches that content today, and this is what
// says so about the real corpus rather than about a hand-built record.
func TestTelemetryReplayCarriesNoRecordedContent(t *testing.T) {
	events, _, _ := replayThroughChain(t, true)
	if len(events) == 0 {
		t.Fatal("no events; the absence below would be vacuous")
	}
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)

	// Sentinels drawn from the fixture's own unhandled records. If any appears,
	// a projection somewhere started binding content without a gate.
	for _, key := range []string{
		"api_request_body", "api_response_body", "assistant_response",
		"user_prompt", "tool_result",
	} {
		if strings.Contains(body, key) {
			t.Errorf("recorded content marker %q reached the spool", key)
		}
	}
}

// TestTelemetryReplayIsSilentWhenNotElected is the negative half, and it is
// PRESENCE-ANCHORED on purpose.
//
// "Nothing arrived" is also what a broken chain produces. So this asserts silence
// against the SAME fixture that the positive test above proves yields five turns
// — the only difference between the two runs is the election. Without that
// anchor, deleting the mapper entirely would leave this test green.
func TestTelemetryReplayIsSilentWhenNotElected(t *testing.T) {
	elected, _, _ := replayThroughChain(t, true)
	if len(elected) == 0 {
		t.Fatal("the elected run produced nothing, so the un-elected run's silence proves nothing")
	}

	events, emitted, drops := replayThroughChain(t, false)
	if len(events) != 0 {
		t.Errorf("an un-elected lane spooled %d event(s); two lanes emitting one turn doubles every token count downstream with no id collision and no error", len(events))
	}
	if emitted != 0 {
		t.Errorf("an un-elected lane counted %d emission(s)", emitted)
	}
	if drops["not-elected"] == 0 {
		t.Errorf("the un-elected run counted no not_elected drops, so its silence is indistinguishable from a chain that never ran. counters: %v", drops)
	}
}
