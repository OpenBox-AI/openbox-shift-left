package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/client"
)

// recoveryEmitter fails every Emit, driving the carry-over path.
type recoveryEmitter struct {
	got []client.DevEvent
	err error
}

func (r *recoveryEmitter) Emit(_ context.Context, ev client.DevEvent) (client.Evaluation, error) {
	r.got = append(r.got, ev)
	return client.Evaluation{}, r.err
}

func recoveryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && hookflow.IsRecoveryFile(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestFlushSweepsAbandonedRecovery is the E8-S7 regression guard: carry-over
// left by a thread that has already ended MUST be retried by a later session's
// SessionEnd flush. Before the sweep, only FlushAll touched `.rec*` files and
// nothing in the ambient hook lifecycle invoked it. The sweeping session has a
// DIFFERENT thread id, so a session-scoped retry would not have found it.
func TestFlushSweepsAbandonedRecovery(t *testing.T) {
	dir := t.TempDir()
	ad := New(Identity{DeveloperDID: testDID}, dir)

	_, _ = ad.Observe(HookPreToolUse, &HookEvent{SessionID: "th-A", ToolName: "Bash"})
	offline := &recoveryEmitter{err: errors.New("dial tcp: network is unreachable")}
	if _, err := ad.Flush(context.Background(), "th-A", offline); err != nil {
		t.Fatalf("offline flush should not error (fail-open): %v", err)
	}
	if got := recoveryNames(t, dir); len(got) != 1 {
		t.Fatalf("thread A should have left exactly 1 carry-over file, got %v", got)
	}

	_, _ = ad.Observe(HookPreToolUse, &HookEvent{SessionID: "th-B", ToolName: "Bash"})
	online := &recoveryEmitter{}
	n, err := ad.Flush(context.Background(), "th-B", online)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n != 2 || len(online.got) != 2 {
		t.Fatalf("flush delivered %d (n=%d), want 2 (own + swept carry-over)", len(online.got), n)
	}
	if got := recoveryNames(t, dir); len(got) != 0 {
		t.Fatalf("carry-over should be consumed after a successful sweep, got %v", got)
	}
	if online.got[0].SessionID != "th-B" {
		t.Errorf("own session should drain first, got %s", online.got[0].SessionID)
	}
}

// TestFlushDoesNotBurnRetriesInOnePass guards the snapshot: one flush consumes
// exactly one attempt, rather than walking the carry-over to
// hookflow.MaxRecoveryAttempts because the sweep re-read what the same pass just wrote.
func TestFlushDoesNotBurnRetriesInOnePass(t *testing.T) {
	dir := t.TempDir()
	ad := New(Identity{DeveloperDID: testDID}, dir)
	_, _ = ad.Observe(HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash"})

	offline := &recoveryEmitter{err: errors.New("offline")}
	for want := 1; want <= hookflow.MaxRecoveryAttempts; want++ {
		if _, err := ad.Flush(context.Background(), "th-1", offline); err != nil {
			t.Fatalf("flush %d: %v", want, err)
		}
		got := recoveryNames(t, dir)
		if len(got) != 1 {
			t.Fatalf("after flush %d: want exactly 1 carry-over file, got %v", want, got)
		}
		if !strings.Contains(got[0], ".rec"+strconv.Itoa(want)+"-") {
			t.Fatalf("after flush %d: want a .rec%d- file, got %q", want, want, got[0])
		}
	}
	if _, err := ad.Flush(context.Background(), "th-1", offline); err != nil {
		t.Fatalf("give-up flush: %v", err)
	}
	if got := recoveryNames(t, dir); len(got) != 0 {
		t.Fatalf("past the attempt bound the line should be dropped, got %v", got)
	}
}

// TestSweepRecoveryIgnoresLiveSpool proves the sweep only claims unowned
// carry-over, never a live spool that may belong to a running session.
func TestSweepRecoveryIgnoresLiveSpool(t *testing.T) {
	dir := t.TempDir()
	sp := hookflow.Spool{Dir: dir}
	_ = sp.Append(spoolEvent("l1", "live"))
	if err := sp.Append(spoolEvent("d1", "dead")); err != nil {
		t.Fatal(err)
	}
	// Turn the dead thread's spool into carry-over.
	if err := os.Rename(sp.SessionPath("dead"), filepath.Join(dir, "dead.rec1-cx-abc.jsonl")); err != nil {
		t.Fatal(err)
	}

	var got []string
	n, err := sp.SweepRecovery(context.Background(), func(_ context.Context, ev client.DevEvent) error {
		got = append(got, ev.EventID)
		return nil
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 || strings.Join(got, ",") != "d1" {
		t.Fatalf("sweep should drain only the carry-over, got n=%d %v", n, got)
	}
	if _, err := os.Stat(sp.SessionPath("live")); err != nil {
		t.Errorf("live spool must be left alone, stat err=%v", err)
	}
}

func TestIsRecoveryFile(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"th.rec1-cx-abc.jsonl", true},
		{"th.rec12-cx-abc.jsonl", true},
		{"th.rec-cx-abc.jsonl", true}, // legacy, pre-counter
		{"th.jsonl", false},
		{"th.jsonl.flushing.cx-abc", false},
		// `.reclaim.` contains ".rec" but is a drain in flight, not carry-over.
		{"th.jsonl.flushing.cx-abc.reclaim.cx-def", false},
		{"th.reclaim.cx-def.jsonl", false},
		{"durations", false},
	}
	for _, c := range cases {
		if got := hookflow.IsRecoveryFile(c.name); got != c.want {
			t.Errorf("hookflow.IsRecoveryFile(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestUndeliveredCountSkipsReclaim pins the same distinction on the telemetry
// field: a claimed orphan is being delivered, so it is not "undelivered".
func TestUndeliveredCountSkipsReclaim(t *testing.T) {
	dir := t.TempDir()
	sp := hookflow.Spool{Dir: dir}
	_ = sp.Append(spoolEvent("x1", "th"))
	data, err := os.ReadFile(sp.SessionPath("th"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"th.rec1-cx-abc.jsonl",
		"th.jsonl.flushing.cx-abc.reclaim.cx-def",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := sp.UndeliveredCount(); got != 1 {
		t.Errorf("UndeliveredCount = %d, want 1 (the .rec1 file only)", got)
	}
}
