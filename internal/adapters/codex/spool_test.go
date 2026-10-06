package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

func spoolEvent(id, session string) client.DevEvent {
	return client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       id,
		EventType:     client.EventToolCall,
		SessionID:     session,
		DeveloperDID:  testDID,
		Timestamp:     "2026-07-23T12:00:00Z",
		Tool:          client.Tool{Name: "Bash", Kind: client.ToolShell},
		Span:          &client.Span{SemanticType: "internal", Stage: "started"},
	}
}

func TestSpool_AppendAndDrainSession(t *testing.T) {
	s := hookflow.Spool{Dir: t.TempDir()}
	for _, id := range []string{"e1", "e2", "e3"} {
		if err := s.Append(spoolEvent(id, "th-1")); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	var got []string
	n, err := s.DrainSession(context.Background(), "th-1", func(_ context.Context, ev client.DevEvent) error {
		got = append(got, ev.EventID)
		return nil
	}, hookflow.DrainOptions{Mode: hookflow.Block, AttemptTimeout: hookflow.DeliveryAttemptTimeout})
	if err != nil || n != 3 {
		t.Fatalf("drain = (%d, %v), want (3, nil)", n, err)
	}
	if strings.Join(got, ",") != "e1,e2,e3" {
		t.Errorf("delivery order = %v", got)
	}
	if n, _ := s.DrainSession(context.Background(), "th-1", nil, hookflow.DrainOptions{Mode: hookflow.Block, AttemptTimeout: hookflow.DeliveryAttemptTimeout}); n != 0 {
		t.Errorf("re-drain delivered %d, want 0 (at-most-once)", n)
	}
}

// TestSpool_BudgetCutQueuesRemainderInHeadFile a budget-bounded drain queues
// the undelivered remainder in the session's head file, which a later,
// unbounded drain completes; delivered events are never re-sent (single-
// attempt delivery).
func TestSpool_BudgetCutQueuesRemainderInHeadFile(t *testing.T) {
	dir := t.TempDir()
	s := hookflow.Spool{Dir: dir}
	for _, id := range []string{"e1", "e2", "e3"} {
		if err := s.Append(spoolEvent(id, "th-1")); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var first []string
	n, err := s.DrainSession(ctx, "th-1", func(_ context.Context, ev client.DevEvent) error {
		first = append(first, ev.EventID)
		time.Sleep(400 * time.Millisecond) // burns the rest of ctx's budget
		return nil
	}, hookflow.DrainOptions{Mode: hookflow.Block, AttemptTimeout: 100 * time.Millisecond})
	if n != 1 || err == nil {
		t.Fatalf("cut drain = (%d, %v), want (1, a cutoff error)", n, err)
	}

	if _, err := os.Stat(filepath.Join(dir, "th-1"+hookflow.HeadSuffix)); err != nil {
		t.Fatalf("no head file written; entries=%v", readDirNames(t, dir))
	}

	var recovered []string
	n, err = s.DrainSession(context.Background(), "th-1", func(_ context.Context, ev client.DevEvent) error {
		recovered = append(recovered, ev.EventID)
		return nil
	}, hookflow.DrainOptions{Mode: hookflow.Block, AttemptTimeout: hookflow.DeliveryAttemptTimeout})
	if err != nil || n != 2 {
		t.Fatalf("recovery drain = (%d, %v), want (2, nil)", n, err)
	}
	if strings.Join(first, ",") != "e1" || strings.Join(recovered, ",") != "e2,e3" {
		t.Errorf("first=%v recovered=%v; delivered events must never re-send, tail must survive", first, recovered)
	}
}

func readDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestSpool_FlushAllSkipsDurationStashSubdir(t *testing.T) {
	dir := t.TempDir()
	s := hookflow.Spool{Dir: dir}
	if err := s.Append(spoolEvent("e1", "th-1")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "durations", "th-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	n, err := s.FlushAll(context.Background(), func(context.Context, client.DevEvent) error { return nil })
	if err != nil || n != 1 {
		t.Fatalf("FlushAll = (%d, %v), want (1, nil)", n, err)
	}
}

func TestSpool_SanitizesSessionID(t *testing.T) {
	dir := t.TempDir()
	s := hookflow.Spool{Dir: dir}
	if err := s.Append(spoolEvent("e1", "../../evil/../id")); err != nil {
		t.Fatal(err)
	}
	// Every name the spool writes, not just the first: the directory also holds
	// the sidecar lock, and an unsanitized id would escape through any of them.
	entries, _ := os.ReadDir(dir)
	if len(entries) == 0 {
		t.Fatal("Append wrote nothing")
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "..") || strings.Contains(e.Name(), "/") {
			t.Fatalf("session id not sanitized for the filesystem: %q", e.Name())
		}
	}
}

// TestIsRecoveryFile pins the legacy-detection contract IsRecoveryFile keeps
// after single-attempt delivery: recognizing an OLD binary's carry-over file
// so it can be discarded on sight, never a shape this release writes.
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
		{"th.head.jsonl", false},
		{"durations", false},
	}
	for _, c := range cases {
		if got := hookflow.IsRecoveryFile(c.name); got != c.want {
			t.Errorf("hookflow.IsRecoveryFile(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestUndeliveredCountIsHeadFilesOnly pins the telemetry field's new meaning:
// what is still queued (a head file), never what already failed and was
// ledgered (that is gone, not pending).
func TestUndeliveredCountIsHeadFilesOnly(t *testing.T) {
	dir := t.TempDir()
	sp := hookflow.Spool{Dir: dir}
	if err := sp.Append(spoolEvent("x1", "th")); err != nil {
		t.Fatal(err)
	}
	if got := sp.UndeliveredCountFor("th"); got != 1 {
		t.Errorf("UndeliveredCountFor(th) = %d, want 1 (its own tail)", got)
	}

	// A failed delivery is attempted-and-gone, not pending: single-attempt
	// delivery has no carry-over, so nothing is left to count as undelivered.
	if _, err := sp.DrainSession(context.Background(), "th",
		func(context.Context, client.DevEvent) error { return context.DeadlineExceeded },
		hookflow.DrainOptions{Mode: hookflow.Block, AttemptTimeout: hookflow.DeliveryAttemptTimeout}); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := sp.UndeliveredCountFor("th"); got != 0 {
		t.Errorf("UndeliveredCountFor(th) = %d, want 0: a single-attempt failure is ledgered, not pending", got)
	}
	if got := sp.UndeliveredCount(); got != 0 {
		t.Errorf("UndeliveredCount = %d, want 0", got)
	}
}
