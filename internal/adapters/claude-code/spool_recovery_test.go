package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// jsonLine and ev build spool file contents directly, so a test can seed a
// spool or recovery file without going through the mapper.
func jsonLine(e client.DevEvent) ([]byte, error) {
	b, err := json.Marshal(e)
	return append(b, '\n'), err
}

func ev(session, id string) client.DevEvent {
	return client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       id,
		EventType:     client.EventToolCall,
		SessionID:     session,
		DeveloperDID:  testDID,
		Timestamp:     "2026-07-08T12:00:00Z",
		Tool:          client.Tool{Name: "Bash", Kind: client.ToolShell},
	}
}

// recoveryNames lists the carry-over files currently in dir.
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
// left by a session that has already ended MUST be retried by a later session's
// SessionEnd flush.
//
// Before the sweep, the only drain that touched a `.rec*` file was FlushAll,
// which nothing in the ambient hook lifecycle invokes — so a developer working
// offline accumulated one recovery file per session and never re-sent any of
// them. Note the sweeping session has a DIFFERENT id from the one that wrote the
// carry-over: a session-scoped retry would not have found it.
func TestFlushSweepsAbandonedRecovery(t *testing.T) {
	dir := t.TempDir()
	ad := New(Identity{DeveloperDID: testDID}, dir)

	// Session A: offline. Its whole spool carries over.
	_, _ = ad.Observe(HookPreToolUse, &HookEvent{SessionID: "sessA", ToolName: "Bash"})
	offline := &fakeEmitter{err: errors.New("dial tcp: network is unreachable")}
	if _, err := ad.Flush(context.Background(), "sessA", offline); err != nil {
		t.Fatalf("offline flush should not error (fail-open): %v", err)
	}
	if got := recoveryNames(t, dir); len(got) != 1 {
		t.Fatalf("session A should have left exactly 1 carry-over file, got %v", got)
	}

	// Session B, a new session id, comes back online.
	_, _ = ad.Observe(HookPreToolUse, &HookEvent{SessionID: "sessB", ToolName: "Read", ToolInput: []byte(`{"file_path":"x"}`)})
	online := &fakeEmitter{}
	n, err := ad.Flush(context.Background(), "sessB", online)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	// B's own event plus A's carried-over one.
	if n != 2 || len(online.got) != 2 {
		t.Fatalf("flush delivered %d (n=%d), want 2 (own + swept carry-over)", len(online.got), n)
	}
	if got := recoveryNames(t, dir); len(got) != 0 {
		t.Fatalf("carry-over should be consumed after a successful sweep, got %v", got)
	}
	// The ending session's own telemetry is delivered before other sessions'
	// backlog, so the ctx budget is never spent on a stranger first.
	if online.got[0].SessionID != "sessB" {
		t.Errorf("own session should drain first, got %s", online.got[0].SessionID)
	}
}

// TestFlushDoesNotBurnRetriesInOnePass guards the snapshot: a single flush must
// consume exactly ONE retry attempt, not walk the carry-over all the way to
// hookflow.MaxRecoveryAttempts because the sweep re-read what the same pass just wrote.
// Without it, one offline SessionEnd would turn a bounded retry into immediate
// permanent loss.
func TestFlushDoesNotBurnRetriesInOnePass(t *testing.T) {
	dir := t.TempDir()
	ad := New(Identity{DeveloperDID: testDID}, dir)
	_, _ = ad.Observe(HookPreToolUse, &HookEvent{SessionID: "sess", ToolName: "Bash"})

	offline := &fakeEmitter{err: errors.New("offline")}
	for want := 1; want <= hookflow.MaxRecoveryAttempts; want++ {
		if _, err := ad.Flush(context.Background(), "sess", offline); err != nil {
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
	// The (hookflow.MaxRecoveryAttempts+1)th flush is the bounded give-up.
	if _, err := ad.Flush(context.Background(), "sess", offline); err != nil {
		t.Fatalf("give-up flush: %v", err)
	}
	if got := recoveryNames(t, dir); len(got) != 0 {
		t.Fatalf("past the attempt bound the line should be dropped, got %v", got)
	}
}

// TestSweepRecoveryIgnoresLiveSpool proves the sweep only claims unowned
// carry-over. A live `<session>.jsonl` may belong to a session still running,
// and its events are un-flushed rather than undelivered — claiming it would
// deliver another session's telemetry out from under it.
func TestSweepRecoveryIgnoresLiveSpool(t *testing.T) {
	dir := t.TempDir()
	sp := hookflow.Spool{Dir: dir}
	_ = sp.Append(ev("live", "l1"))
	line, _ := jsonLine(ev("dead", "d1"))
	if err := os.WriteFile(filepath.Join(dir, "dead.rec1-cc-abc.jsonl"), line, 0o600); err != nil {
		t.Fatal(err)
	}

	fn, got := drainCollect()
	n, err := sp.SweepRecovery(context.Background(), fn)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 || len(*got) != 1 || (*got)[0].EventID != "d1" {
		t.Fatalf("sweep should drain only the carry-over, got n=%d %v", n, *got)
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
		{"sess.rec1-cc-abc.jsonl", true},
		{"sess.rec12-cc-abc.jsonl", true},
		// Legacy name from before the attempt counter existed: still carry-over.
		{"sess.rec-cc-abc.jsonl", true},
		{"sess.jsonl", false},
		{"sess.jsonl.flushing.cc-abc", false},
		// `.reclaim.` contains ".rec" but is a drain in flight, not carry-over —
		// counting it inflated evidence_undelivered with a file being delivered.
		{"sess.jsonl.flushing.cc-abc.reclaim.cc-def", false},
		{"sess.reclaim.cc-def.jsonl", false},
		{"durations", false},
		{"advisories.jsonl", false},
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
	line, _ := jsonLine(ev("sess", "x1"))
	for _, name := range []string{
		"sess.rec1-cc-abc.jsonl",
		"sess.jsonl.flushing.cc-abc.reclaim.cc-def",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), line, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := sp.UndeliveredCount(); got != 1 {
		t.Errorf("UndeliveredCount = %d, want 1 (the .rec1 file only)", got)
	}
}
