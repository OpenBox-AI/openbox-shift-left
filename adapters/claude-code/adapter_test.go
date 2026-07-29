package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// fakeEmitter records what would be emitted and can be told to fail (to exercise
// the fail-open path) or to return a deny evaluation (to prove observe-only
// ignores it for control flow).
type fakeEmitter struct {
	mu   sync.Mutex
	got  []client.DevEvent
	err  error
	eval client.Evaluation
}

func (f *fakeEmitter) Emit(_ context.Context, e client.DevEvent) (client.Evaluation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, e)
	return f.eval, f.err
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
	ad.Advisory.Path = filepath.Join(dir, "advisories.jsonl") // keep the real HOME clean
	if _, err := ad.Observe(HookPreToolUse, &HookEvent{SessionID: "s1", ToolName: "Bash"}); err != nil {
		t.Fatalf("observe: %v", err)
	}

	// A BLOCK verdict + a transport error must both be swallowed: neither may
	// surface as an error the caller could turn into a block (INV-3).
	em := &fakeEmitter{eval: client.Evaluation{Verdict: client.VerdictBlock}, err: errors.New("network down")}
	n, err := ad.Flush(context.Background(), "s1", em)
	if err != nil {
		t.Fatalf("flush must not surface the emitter error (fail-open), got %v", err)
	}
	if len(em.got) != 1 {
		t.Fatalf("emitter should have been called once, got %d", len(em.got))
	}
	// The count is DELIVERED, not attempted (E8-S7): an undelivered event is
	// carried over to a recovery file for a later flush instead of being
	// silently dropped, so it must not be counted as delivered here.
	if n != 0 {
		t.Fatalf("undelivered event counted as delivered: n=%d, want 0", n)
	}
	recs, _ := filepath.Glob(filepath.Join(dir, "*.rec*.jsonl"))
	if len(recs) != 1 {
		t.Fatalf("undelivered event should be carried over to one recovery file, got %v", recs)
	}
}

// TestFlushRecordsAdvisory proves the Advisory tier (STORY-SL-9): a flush whose
// evaluation carries a BLOCK verdict + a guardrail hit writes ONE advisory record
// (would_block=true, guardrail category present) while the flush neither blocks
// nor errors, and the record leaks no content/secret (INV-1/INV-2).
func TestFlushRecordsAdvisory(t *testing.T) {
	dir := t.TempDir()
	secret := "SECRET-COMMAND-do-not-egress"
	ad := New(Identity{DeveloperDID: testDID}, dir)
	advPath := filepath.Join(dir, "advisories.jsonl")
	ad.Advisory.Path = advPath

	// Observe a tool call carrying content in tool_input (stripped before egress).
	if _, err := ad.Observe(HookPreToolUse, &HookEvent{
		SessionID: "s1", ToolName: "Bash", ToolInput: []byte(`{"command":"` + secret + `"}`),
	}); err != nil {
		t.Fatalf("observe: %v", err)
	}

	risk := 0.9
	em := &fakeEmitter{eval: client.Evaluation{
		Verdict:   client.VerdictBlock,
		RiskScore: risk,
		TrustTier: "3",
		Guardrail: &client.GuardrailResult{
			Passed:  false,
			Reasons: []client.GuardrailReason{{Type: "pii", Field: "email", Reason: "Contains PII"}},
		},
	}}
	if _, err := ad.Flush(context.Background(), "s1", em); err != nil {
		t.Fatalf("flush must not error (INV-3): %v", err)
	}

	raw, err := os.ReadFile(advPath)
	if err != nil {
		t.Fatalf("advisory sink not written: %v", err)
	}
	lines := 0
	for _, l := range splitNonEmpty(raw) {
		lines++
		var rec advisoryRecord
		if err := json.Unmarshal(l, &rec); err != nil {
			t.Fatalf("advisory record is not valid JSON: %v\n%s", err, l)
		}
		if rec.Verdict != "BLOCK" || !rec.WouldBlock {
			t.Errorf("record verdict=%q would_block=%t, want BLOCK/true", rec.Verdict, rec.WouldBlock)
		}
		if len(rec.GuardrailReasons) != 1 || rec.GuardrailReasons[0].Type != "pii" {
			t.Errorf("guardrail category missing: %+v", rec.GuardrailReasons)
		}
		if rec.EventType != "ToolCall" {
			t.Errorf("event_type = %q, want ToolCall", rec.EventType)
		}
	}
	if lines != 1 {
		t.Fatalf("want exactly one advisory record, got %d", lines)
	}
	// INV-1/INV-2: no tool content and no secret substring in the sink.
	if strings.Contains(string(raw), secret) {
		t.Fatalf("INV-2 violation: content leaked into advisory sink: %s", raw)
	}
}

func splitNonEmpty(data []byte) [][]byte {
	var out [][]byte
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, []byte(l))
		}
	}
	return out
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
