package hookflow

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

func readSpooledEvents(t *testing.T, spoolDir, sessionID string) []client.DevEvent {
	t.Helper()
	data, err := os.ReadFile((Spool{Dir: spoolDir}).SessionPath(sessionID))
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	var out []client.DevEvent
	for _, line := range NonEmptyLines(data) {
		var ev client.DevEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("decode spooled event: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

func clockSeq(times ...time.Time) func() time.Time {
	i := 0
	return func() time.Time {
		t := times[i]
		if i < len(times)-1 {
			i++
		}
		return t
	}
}

func TestDurationStash_PutTakeClear(t *testing.T) {
	d := DurationStash{Dir: t.TempDir()}
	const sess, key, ts = "s1", "k1", "2026-07-15T12:00:00Z"

	if err := d.PutStart(sess, key, ts); err != nil {
		t.Fatalf("PutStart: %v", err)
	}
	if got := d.TakeStart(sess, key); got != ts {
		t.Fatalf("TakeStart = %q, want %q", got, ts)
	}
	if got := d.TakeStart(sess, key); got != "" {
		t.Fatalf("second TakeStart = %q, want empty (record removed on read)", got)
	}
}

func TestDurationStash_TakeMissingIsEmpty(t *testing.T) {
	d := DurationStash{Dir: t.TempDir()}
	if got := d.TakeStart("s1", "never-written"); got != "" {
		t.Fatalf("TakeStart on missing = %q, want empty", got)
	}
}

func TestDurationStash_ClearSessionSweepsUnpaired(t *testing.T) {
	root := t.TempDir()
	d := DurationStash{Dir: root}
	if err := d.PutStart("s1", "orphan", "2026-07-15T12:00:00Z"); err != nil {
		t.Fatalf("PutStart: %v", err)
	}
	d.ClearSession("s1")
	if got := d.TakeStart("s1", "orphan"); got != "" {
		t.Fatalf("TakeStart after clear = %q, want empty", got)
	}
}

func TestDurationStash_EmptyDirIsInert(t *testing.T) {
	var d DurationStash // zero value: Dir==""
	if err := d.PutStart("s1", "k", "2026-07-15T12:00:00Z"); err != nil {
		t.Fatalf("PutStart on empty-dir stash must be a no-op, got %v", err)
	}
	if got := d.TakeStart("s1", "k"); got != "" {
		t.Fatalf("TakeStart on empty-dir stash = %q, want empty", got)
	}
	d.ClearSession("s1") // must not panic
}
