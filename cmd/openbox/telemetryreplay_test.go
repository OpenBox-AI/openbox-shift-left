package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/telemetryemit"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
)

// Telemetryreplay_test.go; the recorded-traffic replay for the :otel: lane.
// TestTelemetryCommandActuallyRecords next door is the socket control: it
// drives the real command over a real port and is skipped on a host that
// cannot bind.

func replayCorpus(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "internal", "telemetry", "testdata", "corpus", "otel-logs.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recorded corpus fixture %s: %v", path, err)
	}
	return raw
}

func replayThroughChain(t *testing.T, elected bool) ([]map[string]any, int, map[string]int) {
	t.Helper()
	spoolDir := t.TempDir()

	em := &telemetryemit.Emitter{
		Spool: hookflow.Spool{Dir: spoolDir},
		Mapper: telemetryemit.New("did:aip:7f3c9b2e-0000-5000-a000-00000000feed",
			telemetryemit.Policy{Elected: func() bool { return elected }}),
		DID:  func() string { return "did:aip:7f3c9b2e-0000-5000-a000-00000000feed" },
		Warn: func(string, ...any) {},
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

	if len(events) != 5 {
		t.Errorf("got %d turns from the recorded export, want 5 (one per recorded api_request)", len(events))
	}

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
// presence-anchored on purpose. So this asserts silence against the same
// fixture that the positive test above proves yields five turns; the only
// difference between the two runs is the election.
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
