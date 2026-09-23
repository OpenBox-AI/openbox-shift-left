package main

import (
	"context"
	"errors"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
)

// TestTelemetryFanOutRefusesARecordCarryingBothSessionKeys: the fan-out routes
// on which session attribute a record carries, and each provider's client
// signs what it is handed. A record carrying both session.id and
// conversation.id names two tools at once; handing it to either client would
// sign one tool's content with the other's identity, so it is refused before
// any emitter sees it. The emitters are nil here on purpose: reaching one
// would panic.
func TestTelemetryFanOutRefusesARecordCarryingBothSessionKeys(t *testing.T) {
	d := &dualProviderTelemetryEmitter{}
	rec := telemetry.Record{Attrs: map[string]string{
		"session.id":      "cc-session",
		"conversation.id": "codex-thread",
	}}
	err := d.Emit(context.Background(), rec)
	if !errors.Is(err, errAmbiguousTelemetryProvider) {
		t.Fatalf("Emit = %v, want errAmbiguousTelemetryProvider", err)
	}
}
