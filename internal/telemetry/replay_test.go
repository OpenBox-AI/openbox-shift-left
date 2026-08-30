package telemetry

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// It carries every event type that session produced, not only the one the
// mapper handles, because "an unrecognized event is skipped, never an error"
// is a claim about this lane's tolerance of a provider addition; it rides a
// beta surface; and a fixture containing only api_request could not test it.

func corpusFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "corpus", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

func replayFixture(t *testing.T) []Record {
	t.Helper()
	cap := &captureEmitter{}
	r, err := New(Config{Addr: "127.0.0.1:0"}, WithEmitter(cap))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.ConsumeLogsJSON(context.Background(), corpusFixture(t, "otel-logs.json")); err != nil {
		t.Fatalf("ConsumeLogsJSON: %v", err)
	}
	return cap.records
}

// TestReplayDecodesTheRecordedExport is the first thing that must hold: the
// committed fixture is still readable by the collector's own unmarshaler.
func TestReplayDecodesTheRecordedExport(t *testing.T) {
	recs := replayFixture(t)
	if len(recs) == 0 {
		t.Fatal("the recorded export decoded to no records")
	}
	t.Logf("decoded %d records from the recorded export", len(recs))
}

// TestReplayCensusCoversEveryRecordedEventType pins what the corpus contained.
func TestReplayCensusCoversEveryRecordedEventType(t *testing.T) {
	seen := map[string]int{}
	for _, r := range replayFixture(t) {
		seen[r.EventName]++
	}

	want := []string{
		"api_refusal", "api_request", "api_request_body", "api_response_body",
		"assistant_response", "hook_execution_complete", "hook_execution_start",
		"hook_registered", "mcp_server_connection", "plugin_loaded",
		"retention_sweep", "skill_activated", "subagent_completed",
		"tool_decision", "tool_result", "user_prompt",
	}
	for _, name := range want {
		if seen[name] == 0 {
			t.Errorf("event type %q is absent from the census", name)
		}
	}
	for name := range seen {
		if name == "" {
			t.Error("a record decoded with no event name; the mapper would have nothing to dispatch on")
		}
	}
	// This lane rides a provider surface on a beta flag, so a regenerated fixture
	// that picks up a seventeenth event type means the provider added something;
	// and the mapper's "an unrecognized event is skipped, never an error"
	// tolerance should be re-read against it rather than assumed to still hold.
	if len(seen) != len(want) {
		t.Errorf("the census found %d event types, the pinned list names %d; a regenerated "+
			"fixture changed what the provider emits", len(seen), len(want))
	}
	t.Logf("census: %d event types, %d records", len(seen), len(replayFixture(t)))
}

// TestReplayMergesAttributesAcrossScopes proves the projection actually merged
// resource-level attributes into each record, which is where the model id and
// the session id come from on some exports.
func TestReplayMergesAttributesAcrossScopes(t *testing.T) {
	for _, r := range replayFixture(t) {
		if r.EventName != "api_request" {
			continue
		}
		for _, key := range []string{"session.id", "model", "input_tokens", "output_tokens"} {
			if r.Attrs[key] == "" {
				t.Errorf("api_request record is missing %q; the mapper needs it", key)
			}
		}
		if r.Timestamp.IsZero() {
			t.Error("api_request record has a zero timestamp; the mapper drops those")
		}
		if got := r.Attrs["service.name"]; got == "" {
			t.Error("resource-level attributes did not merge into the record")
		}
		return
	}
	t.Fatal("no api_request record in the census")
}
