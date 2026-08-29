package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// apiRequestSamples is how many api_request records the telemetry fixture
// carries. More than one because the mapper mints an activity_id per request id,
// and a single-record fixture cannot show that two turns stay distinct — which
// is the property core's dedupe depends on.
const apiRequestSamples = 5

// extractOTel builds ONE OTLP logs payload out of the recorded export.
//
// It carries every event type the corpus contains, not only the one the mapper
// handles. That is deliberate: "an unrecognized event is skipped, not an error"
// is a claim about this lane's tolerance of a provider addition (OD3 rides a beta
// surface), and a fixture containing only api_request could not test it.
func extractOTel(corpus, out string) error {
	f, err := os.Open(filepath.Join(corpus, "otel", "logs.jsonl"))
	if err != nil {
		return err
	}
	defer f.Close()

	var resource json.RawMessage
	byType := map[string][]json.RawMessage{}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var payload struct {
			ResourceLogs []struct {
				Resource  json.RawMessage `json:"resource"`
				ScopeLogs []struct {
					Scope      json.RawMessage   `json:"scope"`
					LogRecords []json.RawMessage `json:"logRecords"`
				} `json:"scopeLogs"`
			} `json:"resourceLogs"`
		}
		if err := json.Unmarshal(sc.Bytes(), &payload); err != nil {
			continue
		}
		for _, rl := range payload.ResourceLogs {
			if resource == nil {
				resource = rl.Resource
			}
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					name := bodyName(lr)
					if name == "" {
						continue
					}
					limit := 1
					if name == "claude_code.api_request" {
						limit = apiRequestSamples
					}
					if len(byType[name]) < limit {
						byType[name] = append(byType[name], lr)
					}
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(byType) == 0 {
		return fmt.Errorf("no log records found under %s", corpus)
	}

	names := make([]string, 0, len(byType))
	for n := range byType {
		names = append(names, n)
	}
	sort.Strings(names)

	// api_request first so a reader of the fixture meets the handled type before
	// the fifteen it skips.
	var records []json.RawMessage
	records = append(records, byType["claude_code.api_request"]...)
	for _, n := range names {
		if n != "claude_code.api_request" {
			records = append(records, byType[n]...)
		}
	}

	doc := map[string]any{
		"resourceLogs": []any{map[string]any{
			"resource": resource,
			"scopeLogs": []any{map[string]any{
				"scope":      map[string]any{"name": "com.anthropic.claude_code.events"},
				"logRecords": records,
			}},
		}},
	}
	return write(filepath.Join(out, "telemetry", "testdata", "corpus", "otel-logs.json"), doc)
}

func bodyName(lr json.RawMessage) string {
	var rec struct {
		Body struct {
			StringValue string `json:"stringValue"`
		} `json:"body"`
	}
	if err := json.Unmarshal(lr, &rec); err != nil {
		return ""
	}
	return rec.Body.StringValue
}
