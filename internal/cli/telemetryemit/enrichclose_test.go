package telemetryemit

import (
	"context"
	"sync"
	"testing"
)

// After Close an Emit delivers at once, metadata-only, and never starts an
// enrichment: the daemon is stopping and its queues are about to close.
func TestEmitAfterCloseDeliversMetadataOnlyWithoutEnriching(t *testing.T) {
	dir := scratchTrace(t)
	s := newSink()
	e := &Emitter{Mapper: codexMapper(), DID: func() string { return testDID }, Deliver: s.deliver,
		Enrich: &Enricher{Outcome: "codex.content", Enrich: func(context.Context, Call) Result {
			t.Error("the enricher ran after Close")
			return Result{Request: "x", Response: "y"}
		}}}
	e.Close()
	if err := e.Emit(context.Background(), codexSSE(nil)); err != nil {
		t.Fatal(err)
	}
	evs := s.waitFor(t, 2)
	for _, ev := range evs {
		if ev.Span.RequestBody != "" || ev.Span.ResponseBody != "" {
			t.Errorf("a body rode after Close: %+v", ev.Span)
		}
	}
	if f := findings(t, dir, "codex.content"); len(f) != 1 || f[0]["reason"] != "shutdown" {
		t.Errorf("findings = %v, want one shutdown", f)
	}
}

// Emit racing Close must be safe (the race detector is the assertion) and every
// pair is delivered either way.
func TestCloseRacesEmitSafely(t *testing.T) {
	scratchTrace(t)
	s := newSink()
	s.got = make(chan struct{}, 4096)
	e := &Emitter{Mapper: codexMapper(), DID: func() string { return testDID }, Deliver: s.deliver,
		Enrich: &Enricher{Outcome: "codex.content", Enrich: func(context.Context, Call) Result { return Result{} }}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = e.Emit(context.Background(), codexSSE(nil))
			}
		}()
	}
	e.Close()
	wg.Wait()
	e.Close()
	if n := len(s.snapshot()); n != 8*20*2 {
		t.Errorf("delivered %d event(s), want %d", n, 8*20*2)
	}
}
