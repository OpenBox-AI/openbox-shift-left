package acceptancetest

import (
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/conformance"
)

// TestModelCallGateActivityIDNamespaceIsDisjoint gives every producer the same request id
// text and requires distinct activity ids: core dedupes on the id, so a shared
// namespace would silently absorb one producer's row into another's.
func TestModelCallGateActivityIDNamespaceIsDisjoint(t *testing.T) {
	const id = "same-id.1"
	zero := 0
	events := map[string]client.DevEvent{
		"llmgate": {EventType: client.EventModelCallRequested, ModelCallRequestID: id},
		"proxy":   {EventType: client.EventTurnStarted, ProxyRequestID: id},
		"otel":    {EventType: client.EventTurnStarted, OtelRequestID: id},
		"gateway": {EventType: client.EventTurnStarted, GatewayRequestID: id},
		"turn":    {EventType: client.EventTurnStarted, TurnIndex: &zero},
		"rollup":  {EventType: client.EventTurnStarted, SessionRollup: true},
		"tool":    {EventType: client.EventToolCall, Tool: client.Tool{Name: id}},
	}
	seen := map[string]string{}
	for name, ev := range events {
		ev.SessionID = "sess-1"
		got := client.WireActivityID(ev)
		if got == "" {
			t.Errorf("%s: no activity id", name)
			continue
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%s and %s both map to activity id %q", name, prev, got)
		}
		seen[got] = name
	}
}

// TestConformanceVersionMatchesTheClient: the conformance tests restate the
// version they stamp, so this is where the two are held together.
func TestConformanceVersionMatchesTheClient(t *testing.T) {
	schema, err := conformance.LoadSchema()
	if err != nil {
		t.Fatal(err)
	}
	if got := schema["x-schema-version"]; got != client.SchemaVersion {
		t.Errorf("schema x-schema-version = %v, client.SchemaVersion = %q", got, client.SchemaVersion)
	}
}
