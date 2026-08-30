package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestSpool_AppendAndFlushSession(t *testing.T) {
	s := hookflow.Spool{Dir: t.TempDir()}
	for _, id := range []string{"e1", "e2", "e3"} {
		if err := s.Append(spoolEvent(id, "th-1")); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	var got []string
	n, err := s.FlushSession(context.Background(), "th-1", func(_ context.Context, ev client.DevEvent) error {
		got = append(got, ev.EventID)
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("flush = (%d, %v), want (3, nil)", n, err)
	}
	if strings.Join(got, ",") != "e1,e2,e3" {
		t.Errorf("delivery order = %v", got)
	}
	if n, _ := s.FlushSession(context.Background(), "th-1", nil); n != 0 {
		t.Errorf("re-flush delivered %d, want 0 (at-most-once)", n)
	}
}

// TestSpool_BudgetCutPersistsRemainderForRecovery a budget-bounded flush
// persists the undelivered remainder to a recovery file that FlushAll later
// completes; delivered events are never re-sent (INV-5 / the CC spool
// contract, AC-6).
func TestSpool_BudgetCutPersistsRemainderForRecovery(t *testing.T) {
	dir := t.TempDir()
	s := hookflow.Spool{Dir: dir}
	for _, id := range []string{"e1", "e2", "e3"} {
		if err := s.Append(spoolEvent(id, "th-1")); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	var first []string
	n, err := s.FlushSession(ctx, "th-1", func(_ context.Context, ev client.DevEvent) error {
		first = append(first, ev.EventID)
		cancel() // budget expires after the first delivery
		return nil
	})
	if n != 1 || err == nil {
		t.Fatalf("cut flush = (%d, %v), want (1, ctx err)", n, err)
	}

	entries, _ := os.ReadDir(dir)
	rec := ""
	for _, e := range entries {
		if strings.Contains(e.Name(), ".rec") {
			rec = e.Name()
		}
	}
	if rec == "" {
		t.Fatalf("no recovery file written; entries=%v", entries)
	}

	var recovered []string
	n, err = s.FlushAll(context.Background(), func(_ context.Context, ev client.DevEvent) error {
		recovered = append(recovered, ev.EventID)
		return nil
	})
	if err != nil || n != 2 {
		t.Fatalf("recovery flush = (%d, %v), want (2, nil)", n, err)
	}
	if strings.Join(first, ",") != "e1" || strings.Join(recovered, ",") != "e2,e3" {
		t.Errorf("first=%v recovered=%v; delivered events must never re-send, tail must survive", first, recovered)
	}
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
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || strings.Contains(entries[0].Name(), "..") || strings.Contains(entries[0].Name(), "/") {
		t.Fatalf("session id not sanitized for the filesystem: %v", entries)
	}
}
