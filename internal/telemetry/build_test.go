package telemetry

import (
	"bytes"
	"context"
	"testing"
)

// TestBuildConstructsAllThreeSignals is the test that should have existed
// before the receiver was ever called "done". This crosses into the real
// factory with no listener anywhere, so the same class of failure is now
// reachable on a host that cannot bind.
func TestBuildConstructsAllThreeSignals(t *testing.T) {
	r, err := New(Config{Addr: "127.0.0.1:8789"}, WithEmitter(nil), WithLogWriter(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	built, err := r.build(context.Background())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Each must be created or its URL path 404s, and a 404 on a configured signal
	// shows up as export errors in the governed tool's own logs; a lane meant to
	// be invisible making noise in the thing it watches.
	if len(built) != 3 {
		t.Fatalf("built %d components, want 3 (logs, traces, metrics)", len(built))
	}
	for i, c := range built {
		if c == nil {
			t.Errorf("component %d is nil", i)
		}
	}
}

// TestReceiverSettingsHasNoNilFields pins the specific defect.
func TestReceiverSettingsHasNoNilFields(t *testing.T) {
	set := receiverSettings(&bytes.Buffer{})
	if set.Logger == nil {
		t.Error("Logger is nil — otlpreceiver's factory clones it during creation and panics")
	}
	if set.TracerProvider == nil {
		t.Error("TracerProvider is nil")
	}
	if set.MeterProvider == nil {
		t.Error("MeterProvider is nil")
	}
}

// TestCollectorDiagnosticsAreNotDiscarded: the collector's own logger writes
// where we point it.
func TestCollectorDiagnosticsAreNotDiscarded(t *testing.T) {
	var buf bytes.Buffer
	set := receiverSettings(&buf)
	set.Logger.Warn("a receiver diagnostic")
	if buf.Len() == 0 {
		t.Error("the collector's logger discards output; a receiver failing internally would say nothing")
	}
}
