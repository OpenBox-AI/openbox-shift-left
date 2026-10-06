package hookflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStatusPersisterWritesTheFirstReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry-delivery-status.json")
	p := NewStatusPersister(path, time.Hour)
	p.Report(3)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Report did not write a file: %v", err)
	}
	var got DeliverStatus
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("status file is not valid JSON: %v\n%s", err, raw)
	}
	if got.Dropped != 3 {
		t.Errorf("Dropped = %d, want 3", got.Dropped)
	}
	if got.Since.IsZero() {
		t.Error("Since must be set on the first write")
	}
}

// TestStatusPersisterRateLimitsUnchangedRepeats is the "at most once per
// interval" half: repeated Report calls with the same value inside the
// interval must not keep rewriting the file.
func TestStatusPersisterRateLimitsUnchangedRepeats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	p := NewStatusPersister(path, time.Hour)
	p.Report(1)
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	p.Report(1)
	p.Report(1)
	second, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Error("Report rewrote the file for a value that had not changed within the interval")
	}
}

// TestStatusPersisterDefersAChangeUntilTheIntervalElapses is the other side
// of the rate limit: a value that changes inside the interval window is not
// dropped, merely deferred to the next Report (or Flush) that lands after
// the interval, so a burst of drops writes the file at most once per
// interval rather than once per drop. Driven by an injected clock rather
// than real sleeps, so it cannot flake under CPU contention.
func TestStatusPersisterDefersAChangeUntilTheIntervalElapses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	clock := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	p := NewStatusPersister(path, time.Minute)
	p.now = func() time.Time { return clock }

	p.Report(0)
	clock = clock.Add(time.Second) // inside the one-minute interval
	p.Report(1)                    // must not land yet

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got DeliverStatus
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Dropped != 0 {
		t.Fatalf("Dropped = %d, want 0: the changed value landed before the interval elapsed", got.Dropped)
	}

	clock = clock.Add(time.Minute) // now past the interval
	p.Report(1)
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Dropped != 1 {
		t.Errorf("Dropped = %d, want 1: a Report after the interval elapsed must pick up the deferred change", got.Dropped)
	}
}

// TestStatusPersisterFlushIgnoresTheRateLimit is the "at drain" half: the
// final count at shutdown must land even if it arrives inside the interval
// window, or the most useful number (the final one) is exactly the one a
// rate limit could eat.
func TestStatusPersisterFlushIgnoresTheRateLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	p := NewStatusPersister(path, time.Hour)
	p.Report(1)
	p.Flush(5)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got DeliverStatus
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Dropped != 5 {
		t.Errorf("Dropped = %d, want 5 (Flush's value, ignoring the rate limit)", got.Dropped)
	}
}

// TestStatusPersisterEmptyPathIsANoOp is what a caller passes when it could
// not resolve the OpenBox home: persistence is best-effort disclosure, never
// load-bearing, so it must degrade to silence rather than an error.
func TestStatusPersisterEmptyPathIsANoOp(t *testing.T) {
	p := NewStatusPersister("", time.Hour)
	p.Report(1)
	p.Flush(1)
	var nilP *StatusPersister
	nilP.Report(1)
	nilP.Flush(1)
}

func TestDeliverStatusPathIsPerLane(t *testing.T) {
	tel := DeliverStatusPath("/home/.openbox", "telemetry")
	trans := DeliverStatusPath("/home/.openbox", "transport")
	if tel == trans {
		t.Fatalf("telemetry and transport must not share one status path: got %q for both", tel)
	}
}
