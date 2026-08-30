package claudecode

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

func drainCollect() (hookflow.FlushFunc, *[]client.DevEvent) {
	var got []client.DevEvent
	return func(_ context.Context, ev client.DevEvent) error {
		got = append(got, ev)
		return nil
	}, &got
}

func derivingMapper(t time.Time) Mapper {
	m := NewMapper(Identity{DeveloperDID: testDID})
	m.Now = func() time.Time { return t }
	return m
}

// TestDeriveID_Deterministic: the same logical event always yields the same
// id; even if the id is recomputed later from the spooled/persisted record.
func TestDeriveID_Deterministic(t *testing.T) {
	clock := time.Date(2026, 7, 13, 9, 30, 15, 123456789, time.UTC)
	m := derivingMapper(clock)
	hookEv := &HookEvent{SessionID: "s1", ToolName: "Edit", ToolInput: json.RawMessage(`{"file_path":"cli/main.go"}`)}

	a, ok := m.Map(HookPreToolUse, hookEv)
	if !ok {
		t.Fatal("Map ok=false")
	}
	if !strings.HasPrefix(a.EventID, "cc-") {
		t.Errorf("id lost the cc- prefix: %q", a.EventID)
	}

	b, _ := m.Map(HookPreToolUse, hookEv)
	if a.EventID != b.EventID {
		t.Errorf("non-deterministic id: %q vs %q", a.EventID, b.EventID)
	}

	line, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var restored client.DevEvent
	if err := json.Unmarshal(line, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := deriveID(restored); got != a.EventID {
		t.Errorf("id not reproducible from the spooled record: derived %q, stored %q", got, a.EventID)
	}
}

// TestDeriveID_DistinctEventsNeverCollide is the collision-freedom property:
// any two events that differ in ANY structural distinguisher (session, type,
// tool, timestamp down to the nanosecond, or file locator) get distinct ids. A
// base event plus a set of single-field mutations must all hash uniquely.
func TestDeriveID_DistinctEventsNeverCollide(t *testing.T) {
	base := client.DevEvent{
		SessionID: "s1",
		EventType: client.EventToolCall,
		Tool:      client.Tool{Name: "Edit", Kind: client.ToolFile},
		Timestamp: "2026-07-13T09:30:15.123456789Z",
		Span:      &client.Span{FilePath: "a.go", Function: ""},
	}
	mutate := func(f func(*client.DevEvent)) client.DevEvent {
		e := base
		s := *base.Span
		e.Span = &s
		f(&e)
		return e
	}
	variants := []client.DevEvent{
		base,
		mutate(func(e *client.DevEvent) { e.SessionID = "s2" }),
		mutate(func(e *client.DevEvent) { e.EventType = client.EventToolResult }),
		mutate(func(e *client.DevEvent) { e.Tool.Name = "Write" }),
		mutate(func(e *client.DevEvent) { e.Timestamp = "2026-07-13T09:30:15.123456790Z" }),
		mutate(func(e *client.DevEvent) { e.Span.FilePath = "b.go" }),
		mutate(func(e *client.DevEvent) { e.Span.Function = "run" }),
		mutate(func(e *client.DevEvent) { e.Tool.Name = "Edit\x1f"; e.Timestamp = "X" + base.Timestamp }),
	}
	seen := map[string]int{}
	for i, v := range variants {
		id := deriveID(v)
		if prev, dup := seen[id]; dup {
			t.Errorf("collision: variant %d and %d produced the same id %q", prev, i, id)
		}
		seen[id] = i
	}
}

// TestDeriveID_NoContentInID guards INV-2 on the derived-id path: content
// present in a hook's tool_input must not appear (even hashed-adjacent) in the
// event_id. DeriveID hashes only structural fields, so the opaque digest never
// echoes it.
func TestDeriveID_NoContentInID(t *testing.T) {
	m := derivingMapper(time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC))
	secret := "SUPER-SECRET-should-not-appear"
	got, ok := m.Map(HookPreToolUse, &HookEvent{
		SessionID: "s1", ToolName: "Write",
		ToolInput: json.RawMessage(`{"file_path":"/tmp/x","content":"` + secret + `"}`),
	})
	if !ok {
		t.Fatal("Map ok=false")
	}
	if strings.Contains(got.EventID, secret) {
		t.Fatalf("content leaked into event_id: %q", got.EventID)
	}
}

// TestEventID_StableAcrossSpoolLifecycle proves the id is stable through the
// full delivery path: Map → spool → rotate → flush, then a ctx-cut drain →
// recovery file → re-drain.
func TestEventID_StableAcrossSpoolLifecycle(t *testing.T) {
	m := derivingMapper(time.Date(2026, 7, 13, 9, 30, 0, 0, time.UTC))
	sp := hookflow.Spool{Dir: t.TempDir()}

	var want []string
	for i, tool := range []string{"Read", "Edit", "Bash"} {
		m.Now = func() time.Time { return time.Date(2026, 7, 13, 9, 30, 0, i, time.UTC) }
		e, ok := m.Map(HookPreToolUse, &HookEvent{SessionID: "sess", ToolName: tool})
		if !ok {
			t.Fatalf("map %s", tool)
		}
		if err := sp.Append(e); err != nil {
			t.Fatalf("append: %v", err)
		}
		want = append(want, e.EventID)
	}

	fn, got := drainCollect()
	if _, err := sp.FlushSession(context.Background(), "sess", fn); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(*got) != 3 {
		t.Fatalf("drained %d, want 3", len(*got))
	}
	for i, e := range *got {
		if e.EventID != want[i] {
			t.Errorf("id[%d] mutated across spool: got %q, want %q", i, e.EventID, want[i])
		}
	}
}

// TestEventID_RecoveryNeverReSendsAcked: a drain cut short after acking the
// first event persists only the undelivered tail to a recovery file; the re-
// drain delivers the tail once and never re-sends the acked event; and every
// id is the original derived id (no regeneration on the recovery path).
func TestEventID_RecoveryNeverReSendsAcked(t *testing.T) {
	m := derivingMapper(time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC))
	sp := hookflow.Spool{Dir: t.TempDir()}

	var ids []string
	for i, tool := range []string{"Read", "Edit", "Bash"} {
		m.Now = func() time.Time { return time.Date(2026, 7, 13, 10, 0, 0, i, time.UTC) }
		e, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "sess", ToolName: tool})
		_ = sp.Append(e)
		ids = append(ids, e.EventID)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var acked []string
	fn := func(_ context.Context, e client.DevEvent) error {
		acked = append(acked, e.EventID)
		if len(acked) == 1 {
			cancel()
		}
		return nil
	}
	if _, err := sp.FlushSession(ctx, "sess", fn); err == nil {
		t.Error("expected ctx error on the cut-short drain")
	}
	if len(acked) != 1 || acked[0] != ids[0] {
		t.Fatalf("first drain acked %v, want [%s]", acked, ids[0])
	}

	fn2, got := drainCollect()
	if _, err := sp.FlushAll(context.Background(), fn2); err != nil {
		t.Fatalf("recovery flush: %v", err)
	}
	var reSent []string
	for _, e := range *got {
		if e.EventID == ids[0] {
			reSent = append(reSent, e.EventID)
		}
	}
	if len(reSent) != 0 {
		t.Fatalf("acked event re-sent on recovery (at-most-once broken): %v", reSent)
	}
	if len(*got) != 2 || (*got)[0].EventID != ids[1] || (*got)[1].EventID != ids[2] {
		t.Fatalf("recovery delivered wrong tail: got %v, want [%s %s]", idsOf(*got), ids[1], ids[2])
	}
}

func idsOf(evs []client.DevEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.EventID
	}
	return out
}
