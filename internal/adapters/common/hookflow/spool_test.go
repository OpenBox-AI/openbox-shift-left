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
// killed drain is re-drained by FlushAll, not stranded forever. Aged past
// ReclaimOrphanAfter, because that is now what makes it an orphan rather than
// somebody's live drain; the sibling below holds the other half.
func TestSpoolAdoptsOrphan(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
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
	if n != 1 || len(*got) != 1 || (*got)[0].EventID != "orphan1" {
		t.Fatalf("orphan not adopted: n=%d got=%v", n, *got)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan should be consumed, stat err=%v", err)
	}
}

// TestASweepDoesNotReclaimALiveDrainsFile is the other half of the orphan rule,
// and it is a duplicate-delivery guard rather than a tidiness one. drainFile
// holds its rotated file for the whole delivery loop; a sweep that renames it
// away re-delivers every line, and the control plane does not dedupe on event
// id, so those are duplicate governance rows and an activity_id with four.
func TestASweepDoesNotReclaimALiveDrainsFile(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	// Exactly what drainFile leaves behind mid-delivery: freshly stamped.
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

// TestADeadlineDoesNotBurnADeliveryAttempt: the bound exists for events the
// server will never accept. A budget expiry refused nothing, so counting it
// turns a retry cap into a timer that deletes any backlog too big for one
// budget -- which is every backlog the sweep exists to rescue.
func TestADeadlineDoesNotBurnADeliveryAttempt(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	for _, id := range []string{"a", "b", "c"} {
		if err := sp.Append(ev("sess", id)); err != nil {
			t.Fatal(err)
		}
	}

	// Far more passes than MaxRecoveryAttempts, every one of them timing out with
	// nothing refused.
	for pass := range MaxRecoveryAttempts * 3 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // expired before the first line, as a spent budget is
		if _, err := sp.FlushAll(ctx, func(context.Context, client.DevEvent) error { return nil }); err == nil {
			t.Fatalf("pass %d: expected the deadline error", pass)
		}
	}

	if got := sp.DiscardedCount(); got != 0 {
		t.Errorf("%d event(s) discarded by deadlines alone; nothing ever refused them", got)
	}
	if got := sp.BacklogCount(); got != 3 {
		t.Errorf("backlog = %d, want the 3 events still waiting", got)
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

// writeRecoveryFile writes a carry-over file directly under name, one line per
// id, bypassing the production writer so the test controls the exact
// filename -- including shapes the writer itself never produces.
func writeRecoveryFile(t *testing.T, dir, name string, ids ...string) {
	t.Helper()
	var buf []byte
	for _, id := range ids {
		line, err := jsonLine(ev("irrelevant", id))
		if err != nil {
			t.Fatalf("jsonLine: %v", err)
		}
		buf = append(buf, line...)
	}
	if err := os.WriteFile(filepath.Join(dir, name), buf, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestUndeliveredCountForIsolatesBySession is the fix itself: another
// session's carry-over sitting in the same directory must never inflate this
// session's count, which is what a directory-wide sum did.
func TestUndeliveredCountForIsolatesBySession(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	writeRecoveryFile(t, dir, "sessA.rec1-aaa.jsonl", "a1", "a2")
	writeRecoveryFile(t, dir, "sessB.rec1-bbb.jsonl", "b1", "b2", "b3")

	if got := sp.UndeliveredCountFor("sessA"); got != 2 {
		t.Errorf("sessA count = %d, want 2 (its own file only)", got)
	}
	if got := sp.UndeliveredCountFor("sessB"); got != 3 {
		t.Errorf("sessB count = %d, want 3 (its own file only)", got)
	}
	if got := sp.UndeliveredCountFor("sessC"); got != 0 {
		t.Errorf("a session with no carry-over of its own must report 0, got %d", got)
	}
}

// TestUndeliveredCountForCountsLegacyName the pre-attempt-counter carry-over
// name (`<session>.rec-<id>.jsonl`, written before the counter existed) must
// count exactly like a `.rec<N>` one.
func TestUndeliveredCountForCountsLegacyName(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	writeRecoveryFile(t, dir, "sess.rec-legacy.jsonl", "l1")

	if got := sp.UndeliveredCountFor("sess"); got != 1 {
		t.Errorf("legacy carry-over not counted: got %d, want 1", got)
	}
}

// TestUndeliveredCountForIgnoresNonRecoveryFileWithMatchingPrefix a filename
// sharing the session's prefix is not enough on its own; it must also satisfy
// IsRecoveryFile, or any plain file that happens to start with
// "<session>.rec" would inflate the count.
func TestUndeliveredCountForIgnoresNonRecoveryFileWithMatchingPrefix(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	// Shares the "sess.rec" prefix, but the run before '-' is not all digits
	// (an unparsable attempt), so IsRecoveryFile rejects it.
	writeRecoveryFile(t, dir, "sess.recX-abc.jsonl", "n1", "n2")

	if got := sp.UndeliveredCountFor("sess"); got != 0 {
		t.Errorf("a non-recovery file with a matching prefix was counted: got %d, want 0", got)
	}
}

// TestUndeliveredCountForSanitizesSessionID both the writer's stem and the
// query prefix run the raw session id through sanitizeSessionID, so a raw id
// containing a character it rewrites must still match its own carry-over.
func TestUndeliveredCountForSanitizesSessionID(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	rawID := "team/alpha" // sanitizeSessionID rewrites '/' to '_'
	writeRecoveryFile(t, dir, sanitizeSessionID(rawID)+".rec1-ccc.jsonl", "c1", "c2", "c3", "c4")

	if got := sp.UndeliveredCountFor(rawID); got != 4 {
		t.Errorf("sanitized-id count = %d, want 4", got)
	}
}

// TestUndeliveredCountForMissingDirectoryReportsZero this feeds a telemetry
// field and must never fail a session, even when the spool directory itself
// does not exist.
func TestUndeliveredCountForMissingDirectoryReportsZero(t *testing.T) {
	sp := Spool{Dir: filepath.Join(t.TempDir(), "does-not-exist")}
	if got := sp.UndeliveredCountFor("sess"); got != 0 {
		t.Errorf("missing dir = %d, want 0", got)
	}
}
