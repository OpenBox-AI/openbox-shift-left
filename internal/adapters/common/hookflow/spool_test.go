package hookflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	n, err := sp.DrainSession(context.Background(), "sess", fn, DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n != 3 || len(*got) != 3 {
		t.Fatalf("drained %d events (n=%d), want 3", len(*got), n)
	}
	if (*got)[0].EventID != "e1" || (*got)[2].EventID != "e3" {
		t.Errorf("order not preserved: %v", *got)
	}
	if _, err := os.Stat(sp.SessionPath("sess")); !os.IsNotExist(err) {
		t.Errorf("spool file should be gone after drain, stat err=%v", err)
	}
	n2, _ := sp.DrainSession(context.Background(), "sess", fn, DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout})
	if n2 != 0 {
		t.Errorf("re-drain should deliver 0, got %d", n2)
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
	n, err := sp.DrainSession(context.Background(), "sess", fn, DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n != 2 || len(*got) != 2 {
		t.Fatalf("expected 2 good events past the corrupt line, got n=%d len=%d", n, len(*got))
	}
	if got := sp.DiscardedCount(); got != 1 {
		t.Errorf("DiscardedCount = %d, want 1 (the corrupt line)", got)
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

// TestACutPassQueuesTheRemainderInAHeadFile is a regression guard: a
// drain whose remaining budget drops below attemptTimeout before it can start
// another delivery must NOT drop the undelivered tail, and must NOT attempt
// it either (a cut is not a failure). It persists to <stem>.head.jsonl, which
// the next pass drains first, ahead of anything newly appended to the tail.
func TestACutPassQueuesTheRemainderInAHeadFile(t *testing.T) {
	sp := Spool{Dir: t.TempDir()}
	for _, id := range []string{"e1", "e2", "e3", "e4"} {
		_ = sp.Append(ev("sess", id))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	var delivered []string
	var failed int
	sp.OnFailure = func(client.DevEvent, error) { failed++ }
	fn := func(_ context.Context, e client.DevEvent) error {
		delivered = append(delivered, e.EventID)
		if len(delivered) == 2 {
			time.Sleep(500 * time.Millisecond) // burns the rest of ctx's budget
		}
		return nil
	}
	n, err := sp.DrainSession(ctx, "sess", fn, DrainOptions{Mode: Block, AttemptTimeout: 150 * time.Millisecond})
	if err == nil {
		t.Error("expected a cutoff error on the budget-limited drain")
	}
	if n != 2 || len(delivered) != 2 {
		t.Fatalf("delivered %d before the cutoff, want 2", len(delivered))
	}
	if failed != 0 {
		t.Errorf("a cutoff must never be scored as a delivery failure, got %d", failed)
	}

	if _, err := os.Stat(filepath.Join(sp.Dir, "sess"+HeadSuffix)); err != nil {
		t.Fatalf("head file not written: %v", err)
	}
	fn2, got := drainCollect()
	n2, err := sp.DrainSession(context.Background(), "sess", fn2, DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout})
	if err != nil {
		t.Fatalf("recovery drain: %v", err)
	}
	if n2 != 2 || len(*got) != 2 {
		t.Fatalf("recovered %d events, want 2 (the queued tail)", n2)
	}
	if (*got)[0].EventID != "e3" || (*got)[1].EventID != "e4" {
		t.Errorf("recovered wrong events (re-delivery of e1/e2?): %v", *got)
	}
}

// TestAnAgedOrphanIsDiscardedNotRedelivered: a `.flushing.` rotation nobody
// claimed is a drainer that died mid-pass, and its lines' outcome cannot be
// proven -- so, unlike the pre-single-attempt release, it is never
// re-drained. It is discarded, and OnFailure still runs per parseable line so
// a later caller's own latch can halt the run it belonged to.
func TestAnAgedOrphanIsDiscardedNotRedelivered(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	var failed []string
	var failErrs []error
	sp.OnFailure = func(ev client.DevEvent, err error) {
		failed = append(failed, ev.EventID)
		failErrs = append(failErrs, err)
		// Still writes the ledger line a real OnFailure (the default, or a
		// later, latch-writing one) would: this override exists only to
		// observe the per-line call, never to silently replace it.
		sp.defaultOnFailure(ev, err)
	}
	orphan := filepath.Join(dir, "sess.jsonl.flushing.cc-deadbeef")
	line, _ := jsonLine(ev("sess", "orphan1"))
	if err := os.WriteFile(orphan, line, 0o600); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-2 * ReclaimOrphanAfter)
	if err := os.Chtimes(orphan, aged, aged); err != nil {
		t.Fatal(err)
	}
	fn, got := drainCollect()
	n, err := sp.FlushAll(context.Background(), fn)
	if err != nil {
		t.Fatalf("flushall: %v", err)
	}
	if n != 0 || len(*got) != 0 {
		t.Fatalf("an orphan must never be redelivered, got n=%d got=%v", n, *got)
	}
	if len(failed) != 1 || failed[0] != "orphan1" {
		t.Fatalf("OnFailure not invoked for the orphan's own line, got %v", failed)
	}
	if len(failErrs) != 1 || failErrs[0] == nil {
		t.Fatalf("OnFailure err must not be nil for an orphaned line, got %v", failErrs)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan should be consumed, stat err=%v", err)
	}
	if got := sp.DiscardedCount(); got != 1 {
		t.Errorf("DiscardedCount = %d, want 1", got)
	}
}

// TestASweepDoesNotReclaimALiveDrainsFile is the other half of the orphan
// rule: collectSession holds its rotated file for the whole delivery loop, and
// a sweep that treats it as an aged orphan mid-drain would redeliver every
// line for real (nothing downstream dedupes on event_id).
func TestASweepDoesNotReclaimALiveDrainsFile(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	// Exactly what collectSession leaves behind mid-delivery: freshly stamped.
	live := filepath.Join(dir, "sess.jsonl.flushing.cc-inflight")
	line, _ := jsonLine(ev("sess", "inflight1"))
	if err := os.WriteFile(live, line, 0o600); err != nil {
		t.Fatal(err)
	}

	fn, got := drainCollect()
	n, err := sp.FlushAll(context.Background(), fn)
	if err != nil {
		t.Fatalf("flushall: %v", err)
	}
	if n != 0 || len(*got) != 0 {
		t.Fatalf("a sweep re-delivered %d event(s) from a drain still holding them: %v", n, *got)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("the live drain's file was taken from under it: %v", err)
	}
}

// TestALegacyCarryOverFileIsDiscardedOnSight guards the OTHER half of "no
// carry-over": a `<session>.recN-*.jsonl` an old binary left
// behind is recognized (IsRecoveryFile) and discarded the first time this
// release's code sees it, never drained through fn.
func TestALegacyCarryOverFileIsDiscardedOnSight(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	line, _ := jsonLine(ev("sess", "legacy1"))
	legacy := filepath.Join(dir, "sess.rec3-oldbinary.jsonl")
	if err := os.WriteFile(legacy, line, 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := sp.FlushAll(context.Background(), func(context.Context, client.DevEvent) error {
		t.Error("a legacy carry-over file must never reach the emitter")
		return nil
	})
	if err != nil || n != 0 {
		t.Fatalf("flushall = (%d, %v), want (0, nil)", n, err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy carry-over should be gone, stat err=%v", err)
	}
	if got := sp.DiscardedCount(); got != 1 {
		t.Errorf("DiscardedCount = %d, want 1", got)
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

// TestUndeliveredCountForIsHeadPlusTail is the new contract (SessionEnd's
// EvidenceState.Undelivered): it counts what is still QUEUED for a session --
// its head file plus its tail -- never what already failed and was ledgered
// (that is gone, not pending), and never another session's backlog sitting in
// the same directory.
func TestUndeliveredCountForIsHeadPlusTail(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	_ = sp.Append(ev("sessA", "a1"))
	_ = sp.Append(ev("sessA", "a2"))
	sp.writeHead("sessB", [][]byte{[]byte(`{"event_id":"b1"}`)})

	if got := sp.UndeliveredCountFor("sessA"); got != 2 {
		t.Errorf("sessA count = %d, want 2 (its own tail)", got)
	}
	if got := sp.UndeliveredCountFor("sessB"); got != 1 {
		t.Errorf("sessB count = %d, want 1 (its own head file)", got)
	}
	if got := sp.UndeliveredCountFor("sessC"); got != 0 {
		t.Errorf("a session with nothing queued must report 0, got %d", got)
	}
	if got := sp.UndeliveredCount(); got != 1 {
		t.Errorf("directory-wide UndeliveredCount = %d, want 1 (head files only)", got)
	}
}

func TestUndeliveredCountForMissingDirectoryReportsZero(t *testing.T) {
	sp := Spool{Dir: filepath.Join(t.TempDir(), "does-not-exist")}
	if got := sp.UndeliveredCountFor("sess"); got != 0 {
		t.Errorf("missing dir = %d, want 0", got)
	}
}
