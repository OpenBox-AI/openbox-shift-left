package claudecode

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// fakeEmitter records what would be emitted and can be told to fail (to exercise
// the fail-open path) or to return a deny verdict (to prove observe-only ignores
// it).
type fakeEmitter struct {
	mu      sync.Mutex
	got     []client.DevEvent
	err     error
	verdict client.Verdict
}

func (f *fakeEmitter) Emit(_ context.Context, e client.DevEvent) (client.Verdict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, e)
	return f.verdict, f.err
}

func TestObserveThenFlush(t *testing.T) {
	dir := t.TempDir()
	ad := New(Identity{DeveloperDID: testDID}, dir)
	ad.Mapper.NewID = func() string { return "id" }

	hooks := []struct {
		h HookName
		e *HookEvent
	}{
		{HookSessionStart, &HookEvent{SessionID: "s1", Cwd: "/r"}},
		{HookPreToolUse, &HookEvent{SessionID: "s1", ToolName: "Bash"}},
		{HookPostToolUse, &HookEvent{SessionID: "s1", ToolName: "Bash"}},
	}
	for _, h := range hooks {
		spooled, err := ad.Observe(h.h, h.e)
		if err != nil || !spooled {
			t.Fatalf("observe %s = (%v,%v), want (true,nil)", h.h, spooled, err)
		}
	}

	em := &fakeEmitter{}
	n, err := ad.Flush(context.Background(), "s1", em)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n != 3 || len(em.got) != 3 {
		t.Fatalf("flushed %d (n=%d), want 3", len(em.got), n)
	}
}

func TestObserveDropsUnusable(t *testing.T) {
	dir := t.TempDir()
	ad := New(Identity{DeveloperDID: "bad-did"}, dir)
	spooled, err := ad.Observe(HookSessionStart, &HookEvent{SessionID: "s1"})
	if err != nil || spooled {
		t.Fatalf("observe with bad DID = (%v,%v), want (false,nil)", spooled, err)
	}
	// Nothing spooled.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected empty spool dir, got %d entries", len(entries))
	}
}

// TestFlushIsObserveOnly proves the adapter neither blocks nor errors when the
// emitter returns a deny verdict or a transport error — the whole point of
// observe-only + fail-open (INV-3 / D7).
func TestFlushIsObserveOnly(t *testing.T) {
	dir := t.TempDir()
	ad := New(Identity{DeveloperDID: testDID}, dir)
	if _, err := ad.Observe(HookPreToolUse, &HookEvent{SessionID: "s1", ToolName: "Bash"}); err != nil {
		t.Fatalf("observe: %v", err)
	}

	// A BLOCK verdict + a transport error must both be swallowed.
	em := &fakeEmitter{verdict: client.VerdictBlock, err: errors.New("network down")}
	n, err := ad.Flush(context.Background(), "s1", em)
	if err != nil {
		t.Fatalf("flush must not surface the emitter error (fail-open), got %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 event attempted, got %d", n)
	}
	if len(em.got) != 1 {
		t.Fatalf("emitter should have been called once, got %d", len(em.got))
	}
}

func TestFlushAllAcrossSessions(t *testing.T) {
	dir := t.TempDir()
	ad := New(Identity{DeveloperDID: testDID}, dir)
	_, _ = ad.Observe(HookPreToolUse, &HookEvent{SessionID: "s1", ToolName: "Bash"})
	_, _ = ad.Observe(HookPreToolUse, &HookEvent{SessionID: "s2", ToolName: "Read", ToolInput: []byte(`{"file_path":"x"}`)})

	em := &fakeEmitter{}
	n, err := ad.FlushAll(context.Background(), em)
	if err != nil {
		t.Fatalf("flushall: %v", err)
	}
	if n != 2 {
		t.Fatalf("flushall n=%d, want 2", n)
	}
}
