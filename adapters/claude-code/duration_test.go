package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// readSpooledEvents reads the events a session spooled (one JSON line each).
func readSpooledEvents(t *testing.T, spoolDir, sessionID string) []client.DevEvent {
	t.Helper()
	data, err := os.ReadFile((Spool{Dir: spoolDir}).sessionPath(sessionID))
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	var out []client.DevEvent
	for _, line := range nonEmptyLines(data) {
		var ev client.DevEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("decode spooled event: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

// clockSeq returns a Now func that yields the given times in order, holding the
// last one (so extra reads never panic).
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

// --- durationStash (direct) ---------------------------------------------------

func TestDurationStash_PutTakeClear(t *testing.T) {
	d := durationStash{Dir: t.TempDir()}
	const sess, key, ts = "s1", "k1", "2026-07-15T12:00:00Z"

	if err := d.putStart(sess, key, ts); err != nil {
		t.Fatalf("putStart: %v", err)
	}
	if got := d.takeStart(sess, key); got != ts {
		t.Fatalf("takeStart = %q, want %q", got, ts)
	}
	// take removes the record: a second take is empty.
	if got := d.takeStart(sess, key); got != "" {
		t.Fatalf("second takeStart = %q, want empty (record removed on read)", got)
	}
}

func TestDurationStash_TakeMissingIsEmpty(t *testing.T) {
	d := durationStash{Dir: t.TempDir()}
	if got := d.takeStart("s1", "never-written"); got != "" {
		t.Fatalf("takeStart on missing = %q, want empty", got)
	}
}

func TestDurationStash_ClearSessionSweepsUnpaired(t *testing.T) {
	root := t.TempDir()
	d := durationStash{Dir: root}
	if err := d.putStart("s1", "orphan", "2026-07-15T12:00:00Z"); err != nil {
		t.Fatalf("putStart: %v", err)
	}
	d.clearSession("s1")
	// The whole session subdir is gone, so a later take finds nothing.
	if got := d.takeStart("s1", "orphan"); got != "" {
		t.Fatalf("takeStart after clear = %q, want empty", got)
	}
}

func TestDurationStash_EmptyDirIsInert(t *testing.T) {
	var d durationStash // zero value: Dir==""
	if err := d.putStart("s1", "k", "2026-07-15T12:00:00Z"); err != nil {
		t.Fatalf("putStart on empty-dir stash must be a no-op, got %v", err)
	}
	if got := d.takeStart("s1", "k"); got != "" {
		t.Fatalf("takeStart on empty-dir stash = %q, want empty", got)
	}
	d.clearSession("s1") // must not panic
}

// --- Observe threading --------------------------------------------------------

// A PreToolUse then its paired PostToolUse: the spooled ToolResult carries the
// Pre's StartedAt (threaded across processes), so a real duration is recoverable.
func TestThreadDuration_CompletedRecoversStart(t *testing.T) {
	dir := t.TempDir()
	ad := New(Identity{DeveloperDID: testDID}, dir)
	start := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Second)
	ad.Mapper.Now = clockSeq(start, end)

	he := &HookEvent{SessionID: "s1", ToolName: "Read", ToolInput: []byte(`{"file_path":"/a.go"}`)}
	if _, err := ad.Observe(HookPreToolUse, he); err != nil {
		t.Fatalf("observe pre: %v", err)
	}
	if _, err := ad.Observe(HookPostToolUse, he); err != nil {
		t.Fatalf("observe post: %v", err)
	}

	var post *client.DevEvent
	for _, e := range readSpooledEvents(t, dir, "s1") {
		if e.EventType == client.EventToolResult {
			e := e
			post = &e
		}
	}
	if post == nil {
		t.Fatal("no ToolResult spooled")
	}
	wantStart := start.UTC().Format(time.RFC3339Nano)
	if post.StartedAt != wantStart {
		t.Errorf("completed StartedAt = %q, want the Pre start %q (threaded)", post.StartedAt, wantStart)
	}
	if post.EndedAt != end.UTC().Format(time.RFC3339Nano) {
		t.Errorf("completed EndedAt = %q, want %q", post.EndedAt, end.UTC().Format(time.RFC3339Nano))
	}
}

// An unpaired PostToolUse (no matching Pre): threading is a no-op, StartedAt stays
// empty (the documented stash-miss fallback), and nothing panics.
func TestThreadDuration_UnpairedCompletedNoStart(t *testing.T) {
	dir := t.TempDir()
	ad := New(Identity{DeveloperDID: testDID}, dir)
	he := &HookEvent{SessionID: "s1", ToolName: "Bash"}
	if _, err := ad.Observe(HookPostToolUse, he); err != nil {
		t.Fatalf("observe post: %v", err)
	}
	evs := readSpooledEvents(t, dir, "s1")
	if len(evs) != 1 || evs[0].EventType != client.EventToolResult {
		t.Fatalf("want one ToolResult, got %+v", evs)
	}
	if evs[0].StartedAt != "" {
		t.Errorf("unpaired completed StartedAt = %q, want empty", evs[0].StartedAt)
	}
}

// SessionEnd sweeps the session's stash so a tool call whose PostToolUse never
// fired does not leave a record behind.
func TestThreadDuration_SessionEndClearsStash(t *testing.T) {
	dir := t.TempDir()
	ad := New(Identity{DeveloperDID: testDID}, dir)
	// A Pre with no matching Post leaves an orphan record.
	if _, err := ad.Observe(HookPreToolUse, &HookEvent{SessionID: "s1", ToolName: "Bash"}); err != nil {
		t.Fatalf("observe pre: %v", err)
	}
	if _, err := ad.Observe(HookSessionEnd, &HookEvent{SessionID: "s1"}); err != nil {
		t.Fatalf("observe end: %v", err)
	}
	// The session stash subdir is gone.
	if _, err := os.Stat(ad.Durations.sessionDir("s1")); !os.IsNotExist(err) {
		t.Errorf("session stash dir should be removed after SessionEnd, stat err = %v", err)
	}
}

// The durations subdir must not derail FlushAll (it skips subdirectories).
func TestThreadDuration_DoesNotBreakFlushAll(t *testing.T) {
	dir := t.TempDir()
	ad := New(Identity{DeveloperDID: testDID}, dir)
	if _, err := ad.Observe(HookPreToolUse, &HookEvent{SessionID: "s1", ToolName: "Bash"}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	em := &fakeEmitter{}
	n, err := ad.FlushAll(context.Background(), em)
	if err != nil {
		t.Fatalf("flushall: %v", err)
	}
	if n != 1 || len(em.got) != 1 {
		t.Fatalf("flushall n=%d emitted=%d, want 1/1 (durations subdir skipped)", n, len(em.got))
	}
}
