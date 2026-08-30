package telemetry

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// replay_test.go — the recorded-corpus census for this lane's intake.
//
// The fixture is a sanitized OTLP/JSON logs payload lifted from a real desktop
// session (see cli/cmd/corpusfixture). It carries EVERY event type that session
// produced, not only the one the mapper handles, because "an unrecognized event
// is skipped, never an error" is a claim about this lane's tolerance of a
// provider addition — it rides a beta surface — and a fixture containing only
// api_request could not test it.
//
// The entry point is the rule. These records are produced by the PRODUCTION
// projection, not hand-written: a test that constructs Record values and hands
// them to the mapper proves arithmetic on our own struct while claiming to prove
// a mapping of real traffic.

// corpusFixture reads the committed fixture. It fails rather than skips when the
// file is missing: a census that quietly covers nothing is the failure mode this
// whole phase exists to remove.
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
// committed fixture is still readable by the collector's own unmarshaler. It is
// not a formality — the sanitizer rewrites values, and spanId/traceId are
// LENGTH-VALIDATED hex. A sanitizer that replaced one with an ordinary
// placeholder would make every fixture in this directory undecodable, and the
// suites built on them would all skip or fail for a reason unrelated to the
// product.
func TestReplayDecodesTheRecordedExport(t *testing.T) {
	recs := replayFixture(t)
	if len(recs) == 0 {
		t.Fatal("the recorded export decoded to no records")
	}
	t.Logf("decoded %d records from the recorded export", len(recs))
}

// TestReplayCensusCoversEveryRecordedEventType pins what the corpus contained.
//
// The list is written out rather than derived from the fixture, because a census
// that asks the fixture what it contains agrees with itself by construction. If a
// regenerated fixture drops a type, this goes red and somebody decides whether
// the provider stopped emitting it or the extractor stopped selecting it.
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
	// A NEW type appearing is as much a decision as one disappearing. This lane
	// rides a provider surface on a beta flag, so a regenerated fixture that picks
	// up a seventeenth event type means the provider added something — and the
	// mapper's "an unrecognized event is skipped, never an error" tolerance should
	// be re-read against it rather than assumed to still hold.
	if len(seen) != len(want) {
		t.Errorf("the census found %d event types, the pinned list names %d; a regenerated "+
			"fixture changed what the provider emits", len(seen), len(want))
	}
	t.Logf("census: %d event types, %d records", len(seen), len(replayFixture(t)))
}

// TestReplayMergesAttributesAcrossScopes proves the projection actually merged
// resource-level attributes into each record, which is where the model id and the
// session id come from on some exports.
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
