package hookflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

const testDID = "did:aip:7f3c9b2e-0000-5000-a000-000000000001"

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

func drainCollect() (FlushFunc, *[]client.DevEvent) {
	var got []client.DevEvent
	return func(_ context.Context, e client.DevEvent) error {
		got = append(got, e)
		return nil
	}, &got
}

func TestSpoolRoundTrip(t *testing.T) {
	sp := Spool{Dir: t.TempDir()}
	for i, id := range []string{"e1", "e2", "e3"} {
		if err := sp.Append(ev("sess", id)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	fn, got := drainCollect()
	n, err := sp.FlushSession(context.Background(), "sess", fn)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n != 3 || len(*got) != 3 {
		t.Fatalf("drained %d events (n=%d), want 3", len(*got), n)
	}
	if (*got)[0].EventID != "e1" || (*got)[2].EventID != "e3" {
		t.Errorf("order not preserved: %v", *got)
	}
	if _, err := os.Stat(sp.SessionPath("sess")); !os.IsNotExist(err) {
		t.Errorf("spool file should be gone after flush, stat err=%v", err)
	}
	n2, _ := sp.FlushSession(context.Background(), "sess", fn)
	if n2 != 0 {
		t.Errorf("re-flush should drain 0, got %d", n2)
	}
}

func TestSpoolCorruptLineSkipped(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	_ = sp.Append(ev("sess", "good1"))
	f, _ := os.OpenFile(sp.SessionPath("sess"), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("{not valid json\n")
	f.Close()
	_ = sp.Append(ev("sess", "good2"))

	fn, got := drainCollect()
	n, err := sp.FlushSession(context.Background(), "sess", fn)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n != 2 || len(*got) != 2 {
		t.Fatalf("expected 2 good events past the corrupt line, got n=%d len=%d", n, len(*got))
	}
}

func TestFlushAllMultipleSessions(t *testing.T) {
	sp := Spool{Dir: t.TempDir()}
	_ = sp.Append(ev("sessA", "a1"))
	_ = sp.Append(ev("sessB", "b1"))
	_ = sp.Append(ev("sessB", "b2"))

	fn, got := drainCollect()
	n, err := sp.FlushAll(context.Background(), fn)
	if err != nil {
		t.Fatalf("flushall: %v", err)
	}
	if n != 3 || len(*got) != 3 {
		t.Fatalf("flushall drained %d (n=%d), want 3", len(*got), n)
	}
}

func TestFlushAllEmptyDir(t *testing.T) {
	sp := Spool{Dir: filepath.Join(t.TempDir(), "does-not-exist-yet")}
	n, err := sp.FlushAll(context.Background(), func(context.Context, client.DevEvent) error { return nil })
	if err != nil || n != 0 {
		t.Fatalf("empty dir flushall = (%d,%v), want (0,nil)", n, err)
	}
}

// TestSpoolCtxCancelPersistsRemainder is the F1 regression guard: a drain cut
// short by ctx must NOT drop the undelivered tail; it persists to a recovery
// file that a later FlushAll completes, with NO re-delivery of what was sent.
func TestSpoolCtxCancelPersistsRemainder(t *testing.T) {
	sp := Spool{Dir: t.TempDir()}
	for _, id := range []string{"e1", "e2", "e3", "e4"} {
		_ = sp.Append(ev("sess", id))
	}
	ctx, cancel := context.WithCancel(context.Background())
	var delivered []string
	fn := func(_ context.Context, e client.DevEvent) error {
		delivered = append(delivered, e.EventID)
		if len(delivered) == 2 {
			cancel()
		}
		return nil
	}
	n, err := sp.FlushSession(ctx, "sess", fn)
	if err == nil {
		t.Error("expected ctx error on the cut-short drain")
	}
	if n != 2 || len(delivered) != 2 {
		t.Fatalf("delivered %d before cancel, want 2", len(delivered))
	}
	fn2, got := drainCollect()
	n2, err := sp.FlushAll(context.Background(), fn2)
	if err != nil {
		t.Fatalf("recovery flush: %v", err)
	}
	if n2 != 2 || len(*got) != 2 {
		t.Fatalf("recovered %d events, want 2 (the undelivered tail)", n2)
	}
	if (*got)[0].EventID != "e3" || (*got)[1].EventID != "e4" {
		t.Errorf("recovered wrong events (re-delivery?): %v", *got)
	}
}

// TestSpoolAdoptsOrphan is the F2 guard: a `.flushing.<id>` file orphaned by a
// killed drain is re-drained by FlushAll, not stranded forever.
func TestSpoolAdoptsOrphan(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	orphan := filepath.Join(dir, "sess.jsonl.flushing.cc-deadbeef")
	line, _ := jsonLine(ev("sess", "orphan1"))
	if err := os.WriteFile(orphan, line, 0o600); err != nil {
		t.Fatal(err)
	}
	fn, got := drainCollect()
	n, err := sp.FlushAll(context.Background(), fn)
	if err != nil {
		t.Fatalf("flushall: %v", err)
	}
	if n != 1 || len(*got) != 1 || (*got)[0].EventID != "orphan1" {
		t.Fatalf("orphan not adopted: n=%d got=%v", n, *got)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan should be consumed, stat err=%v", err)
	}
}

func TestSanitizeSessionID(t *testing.T) {
	if got := sanitizeSessionID("a/b\\c:d"); got != "a_b_c_d" {
		t.Errorf("sanitize = %q", got)
	}
	if got := sanitizeSessionID(""); got != "unknown" {
		t.Errorf("empty session id → %q, want unknown", got)
	}
	uuid := "7f3c9b2e-0000-5000-a000-000000000001"
	if got := sanitizeSessionID(uuid); got != uuid {
		t.Errorf("uuid mangled: %q", got)
	}
}
