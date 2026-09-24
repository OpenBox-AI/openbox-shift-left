package main

import (
	"context"
	"testing"
	"time"
)

// startRecordingCloser is a shutdownCloser whose Close records the instant
// it was entered (before ever blocking), and then waits out ctx: either its
// own release closes first (reported clean, abandoned=0) or ctx expires
// first (reported abandoned=1). Recording the entry instant is what makes a
// serialized (one-after-another) Close of several of these observably
// different from a concurrent one: serialized entries are spread across the
// whole shutdown; concurrent ones land within microseconds of each other.
type startRecordingCloser struct {
	starts  chan<- time.Time
	release chan struct{}
}

func (c *startRecordingCloser) Close(ctx context.Context) int {
	c.starts <- time.Now()
	select {
	case <-c.release:
		return 0
	case <-ctx.Done():
		return 1
	}
}

// TestCloseAllConcurrentlyStartsEveryCloserAtOnce is the shutdown fix: every
// closer's own Close must be entered at roughly the same instant, not one
// after another. A serialized Close would let an early, slow closer occupy
// the ENTIRE shared ctx deadline before a later one is even asked to start
// waiting on it -- by the time that later Close call is entered, ctx has
// already expired, so it is reported abandoned regardless of how quickly
// its own work would actually have finished given a fair share of the
// budget. Entry-time spread is what distinguishes the two shapes
// deterministically, without depending on any particular closer's own
// internal timing.
func TestCloseAllConcurrentlyStartsEveryCloserAtOnce(t *testing.T) {
	const n = 4
	starts := make(chan time.Time, n)
	closers := make([]shutdownCloser, n)
	for i := 0; i < n; i++ {
		closers[i] = &startRecordingCloser{starts: starts, release: make(chan struct{})}
	}

	// None of the n closers ever release: every one is abandoned once ctx
	// expires, so total elapsed is EXACTLY what reveals serialization --
	// concurrently, one shared wait; serialized, n of them back to back.
	const budget = 60 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	abandoned := closeAllConcurrently(ctx, closers...)
	elapsed := time.Since(start)

	if abandoned != n {
		t.Fatalf("abandoned = %d, want %d (every closer's release was never fired)", abandoned, n)
	}
	// A serialized Close of n closers each waiting out the full budget would
	// take roughly n*budget (~240ms here); concurrently it takes roughly one
	// budget (~60ms). The midpoint is a generous, non-flaky cutoff.
	if want := budget * time.Duration(n) / 2; elapsed >= want {
		t.Fatalf("closeAllConcurrently took %s for %d closers sharing a %s budget; want close to "+
			"one budget's worth (concurrent), not roughly %d of them back to back (serialized)",
			elapsed, n, budget, n)
	}

	close(starts)
	var times []time.Time
	for ts := range starts {
		times = append(times, ts)
	}
	if len(times) != n {
		t.Fatalf("got %d recorded start times, want %d", len(times), n)
	}
	min, max := times[0], times[0]
	for _, ts := range times {
		if ts.Before(min) {
			min = ts
		}
		if ts.After(max) {
			max = ts
		}
	}
	if spread := max.Sub(min); spread > budget/2 {
		t.Errorf("closer entry times spread over %s (budget %s); want them all entered together, "+
			"not one after another", spread, budget)
	}
}

// TestCloseAllConcurrentlySumsEveryResult pins the arithmetic half:
// whichever closers finish cleanly and whichever are abandoned, the total
// is their sum, not merely the first or last result.
func TestCloseAllConcurrentlySumsEveryResult(t *testing.T) {
	starts := make(chan time.Time, 3)
	clean1 := &startRecordingCloser{starts: starts, release: make(chan struct{})}
	clean2 := &startRecordingCloser{starts: starts, release: make(chan struct{})}
	stuck := &startRecordingCloser{starts: starts, release: make(chan struct{})}
	close(clean1.release)
	close(clean2.release)
	// stuck never releases.

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	abandoned := closeAllConcurrently(ctx, clean1, clean2, stuck)

	if abandoned != 1 {
		t.Fatalf("abandoned = %d, want 1 (only the closer that never released)", abandoned)
	}
}
