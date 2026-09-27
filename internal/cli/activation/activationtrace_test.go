package activation

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// TestActivateAndDeactivateTraceTheEnvDiff: each settings env change a lane
// makes is one activation record naming every key set, displaced, removed
// or restored -- with a credential-shaped key's value masked, since a
// developer's env block can hold a token beside the keys a lane owns.
func TestActivateAndDeactivateTraceTheEnvDiff(t *testing.T) {
	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	defer restore()

	home := t.TempDir()
	secret := "sk-" + strings.Repeat("z", 24) // derived here, never real
	seed(t, home, `{"env":{"ANTHROPIC_BASE_URL":"http://old","OTEL_EXPORTER_OTLP_HEADERS":"`+secret+`"}}`)
	activate(t, home, LaneTransport, map[string]string{
		"ANTHROPIC_BASE_URL":         "http://127.0.0.1:8790",
		"OTEL_EXPORTER_OTLP_HEADERS": "x",
	})
	if _, err := Deactivate(home, settingsPath(home), LaneTransport, false); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	recs, _, err := trace.Read(dir, func(r trace.Record) bool { return r.Stage == trace.StageActivation })
	if err != nil || len(recs) != 2 {
		t.Fatalf("Read = %d, %v; want one activate and one deactivate record", len(recs), err)
	}
	raw, _ := json.Marshal(recs)
	if strings.Contains(string(raw), secret) {
		t.Fatalf("a credential-shaped prior value reached the trace: %s", raw)
	}
	for _, want := range []string{"ANTHROPIC_BASE_URL", "http://old", "http://127.0.0.1:8790"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("records do not name %q: %s", want, raw)
		}
	}
	if recs[0].Detail["step"] != "env-activate" || recs[1].Detail["step"] != "env-deactivate" ||
		recs[0].Outcome != "ok" || recs[1].Outcome != "ok" {
		t.Errorf("records = %+v; want ok env-activate then ok env-deactivate", recs)
	}
}
