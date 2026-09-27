package main

import (
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// TestElectedNameFnTracesOnlyOnChange is phase 3's own contract: the
// election is re-resolved on every relayed call, so tracing every
// resolution would flood the trace; only a CHANGE in the elected lane's
// name may produce an election record, and the very first resolution
// always counts as one (there is nothing to compare it against).
func TestElectedNameFnTracesOnlyOnChange(t *testing.T) {
	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	defer restore()

	override := true
	name := electedNameFn("/does/not/exist/settings.json", activation.LaneTransport, &override)

	// Same forced result, called three times: exactly one election record.
	for i := 0; i < 3; i++ {
		if got := name(); got != string(activation.LaneTransport) {
			t.Fatalf("call %d: name() = %q, want %q", i, got, activation.LaneTransport)
		}
	}
	recs, _, err := trace.Read(dir, nil)
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	if n := countStage(recs, trace.StageElection); n != 1 {
		t.Fatalf("election records after 3 unchanged resolutions = %d, want 1: %+v", n, recs)
	}

	// Flip the forced lane: a second, distinct election record.
	override2 := false
	name2 := electedNameFn("/does/not/exist/settings.json", activation.LaneGateway, &override2)
	name2()
	recs, _, err = trace.Read(dir, nil)
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	if n := countStage(recs, trace.StageElection); n != 2 {
		t.Fatalf("election records after a change = %d, want 2: %+v", n, recs)
	}
}

func countStage(recs []trace.Record, stage string) int {
	n := 0
	for _, r := range recs {
		if r.Stage == stage {
			n++
		}
	}
	return n
}
