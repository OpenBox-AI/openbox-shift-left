package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// readSpooledEvents reads the events a session spooled (one JSON line each).
func readSpooledEvents(t *testing.T, spoolDir, sessionID string) []client.DevEvent {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(spoolDir, sessionID+".jsonl"))
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	var out []client.DevEvent
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var ev client.DevEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decode spooled event: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

// clockSeq returns a clock that walks a fixed sequence, then holds the last
// value — so a test can pin distinct start/end times without sleeping.
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
	if _, err := os.Stat(ad.Durations.SessionDir("s1")); !os.IsNotExist(err) {
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
