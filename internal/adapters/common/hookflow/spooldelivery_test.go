package hookflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// deliverySink is an Emitter that can act during a delivery, which is the only
// way to reproduce the measured defect: the appends that go missing are made
// *while* a drain is in flight, after its rename and before its release.
type deliverySink struct {
	mu sync.Mutex
	// during runs before each delivery is recorded, with the delivery index.
	during   func(i int)
	got      []string
	lockSeen []bool
	lockPath string
}

func (s *deliverySink) Emit(_ context.Context, ev client.DevEvent) (client.Evaluation, error) {
	s.mu.Lock()
	i := len(s.got)
	s.got = append(s.got, ev.EventID)
	if s.lockPath != "" {
		_, err := os.Stat(s.lockPath)
		s.lockSeen = append(s.lockSeen, err == nil)
	}
	s.mu.Unlock()
	if s.during != nil {
		s.during(i)
	}
	return client.Evaluation{}, nil
}

func (s *deliverySink) delivered() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

func testEngine(t *testing.T) *Engine {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cc-spool")
	return &Engine{Spool: Spool{Dir: dir}}
}

func spoolEvent(t *testing.T, e *Engine, session, id string) {
	t.Helper()
	ev := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       id,
		EventType:     client.EventTurnCompleted,
		SessionID:     session,
		DeveloperDID:  "did:aip:test",
		Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := e.Spool.Append(ev); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

// TestABurstAppendedDuringADrainIsStillDelivered is *the* regression test for
// this phase, and it reproduces the measured shape rather than a generic
// "an event arrived after a flush".
//
// drainFile renames the session file aside before delivering anything, so an
// append made during the drain lands in a brand-new file the running drain will
// never read; and RealtimeTrigger.Maybe returns silently when it finds the
// flushlock inside the debounce window, so the append cannot spawn a flusher
// either. When such a burst is a session's last activity there is no next
// append, and nothing ever retries them. That is how 118 events sat in a spool.
func TestABurstAppendedDuringADrainIsStillDelivered(t *testing.T) {
	const session = "sess-burst"
	e := testEngine(t)
	spoolEvent(t, e, session, "first")

	const burst = 81 // the measured volley, 81 events inside 0.972s
	sink := &deliverySink{lockPath: e.Spool.FlushLockPath(session)}
	sink.during = func(i int) {
		if i != 0 {
			return
		}
		// Mid-drain: the rotation has happened and the lock is held.
		for n := range burst {
			spoolEvent(t, e, session, fmt.Sprintf("burst-%d", n))
		}
	}

	n, err := e.Flush(context.Background(), session, sink)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if got := len(sink.delivered()); got != burst+1 {
		t.Errorf("delivered %d events, want %d; the burst appended during the drain was stranded", got, burst+1)
	}
	if n != burst+1 {
		t.Errorf("Flush reported %d delivered, want %d", n, burst+1)
	}
	if left := spoolLines(t, e.Spool.SessionPath(session)); left != 0 {
		t.Errorf("%d events remain in the session file after the flush returned", left)
	}
}

// TestTheReDrainHappensBeforeTheLockIsReleased asserts the ORDERING, not just
// the outcome, because the outcome alone cannot tell the fix from a lucky
// retry. Releasing the lock first and re-checking after reopens the same
// window: an append landing in between is debounced by the still-fresh lock and
// stranded again.
//
// The assertion is a filesystem observation and needs no seam: every delivery
// records whether the flushlock existed at that moment.
func TestTheReDrainHappensBeforeTheLockIsReleased(t *testing.T) {
	const session = "sess-order"
	e := testEngine(t)
	spoolEvent(t, e, session, "first")

	sink := &deliverySink{lockPath: e.Spool.FlushLockPath(session)}
	sink.during = func(i int) {
		if i == 0 {
			spoolEvent(t, e, session, "second")
		}
	}

	if _, err := e.Flush(context.Background(), session, sink); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if len(sink.lockSeen) < 2 {
		t.Fatalf("only %d deliveries; the re-drain never happened", len(sink.lockSeen))
	}
	for i, held := range sink.lockSeen {
		if !held {
			t.Errorf("delivery %d ran with no flushlock held; the re-drain moved after the release, "+
				"which restores the stranding window", i)
		}
	}
}

// TestTheReDrainLoopIsBounded a session whose events keep arriving must not
// keep one flusher spinning forever, and what it could not take must be said
// out loud rather than left to look like a clean finish.
func TestTheReDrainLoopIsBounded(t *testing.T) {
	const session = "sess-forever"
	e := testEngine(t)
	var logged strings.Builder
	e.Log = func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) }
	spoolEvent(t, e, session, "first")

	seq := 0
	sink := &deliverySink{}
	sink.during = func(int) {
		seq++
		spoolEvent(t, e, session, fmt.Sprintf("endless-%d", seq))
	}

	done := make(chan int, 1)
	go func() {
		n, _ := e.Flush(context.Background(), session, sink)
		done <- n
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the re-drain loop did not terminate on a file that keeps growing")
	}

	if got := logged.String(); !strings.Contains(got, "remain") {
		t.Errorf("the loop hit its bound and said nothing about what was left behind: %q", got)
	}
	if left := spoolLines(t, e.Spool.SessionPath(session)); left == 0 {
		t.Error("the fixture never actually outran the loop, so the bound was not exercised")
	}
}

// TestAnAbandonedSessionFileIsSweptUp nothing retries a file whose session is
// over: the 1,048 hook-lane events untouched since 2026-08-13 are that gap, and
// they are lane-agnostic, so the net has to run without the originating
// session.
func TestAnAbandonedSessionFileIsSweptUp(t *testing.T) {
	e := testEngine(t)
	spoolEvent(t, e, "gone-1", "a")
	spoolEvent(t, e, "gone-2", "b")

	sink := &deliverySink{}
	n, err := e.FlushAll(context.Background(), sink)
	if err != nil {
		t.Fatalf("FlushAll: %v", err)
	}
	if n != 2 {
		t.Errorf("swept %d events, want 2", n)
	}
	for _, s := range []string{"gone-1", "gone-2"} {
		if left := spoolLines(t, e.Spool.SessionPath(s)); left != 0 {
			t.Errorf("%s still holds %d events after the sweep", s, left)
		}
	}
}

// TestADiscardedBatchIsCountedAndReported loss is acceptable on the observe
// path; unreported loss is not. MaxRecoveryAttempts already stopped
// re-queueing, with no log line, no counter and no telemetry.
func TestADiscardedBatchIsCountedAndReported(t *testing.T) {
	e := testEngine(t)
	var logged strings.Builder
	e.Log = func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) }

	// A carry-over file already at the cap: one more failed drain discards it.
	stem := filepath.Join(e.Spool.Dir, "sess-lost.rec"+fmt.Sprint(MaxRecoveryAttempts)+"-ABCDEF")
	if err := os.MkdirAll(e.Spool.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stem+".jsonl", []byte(`{"event_id":"doomed","openbox_session_id":"sess-lost"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	refusing := &refusingSink{}
	if _, err := e.FlushAll(context.Background(), refusing); err == nil {
		t.Log("FlushAll reported no error, which is fine; the discard is what matters")
	}

	if got := e.Spool.DiscardedCount(); got != 1 {
		t.Errorf("DiscardedCount = %d, want 1; a discarded batch must be counted", got)
	}
	if got := logged.String(); !strings.Contains(got, "discard") {
		t.Errorf("a discarded batch produced no log line: %q", got)
	}
}

// refusingSink fails every delivery, so lines are always re-queued.
type refusingSink struct{}

func (refusingSink) Emit(context.Context, client.DevEvent) (client.Evaluation, error) {
	return client.Evaluation{}, fmt.Errorf("sink refuses everything")
}

// TestAFilePastTheRetirementAgeIsDeletedAndReported deleting evidence is
// irreversible in a governance product, so the deletion has to be loud enough
// that an operator notices before an auditor does.
func TestAFilePastTheRetirementAgeIsDeletedAndReported(t *testing.T) {
	e := testEngine(t)
	var logged strings.Builder
	e.Log = func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) }

	spoolEvent(t, e, "ancient", "old-1")
	path := e.Spool.SessionPath("ancient")
	old := time.Now().Add(-RetireSpoolAfter - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Retire(context.Background()); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a file past the retirement age survived")
	}
	got := logged.String()
	if !strings.Contains(got, "ancient") || !strings.Contains(strings.ToLower(got), "retir") {
		t.Errorf("the retirement was not reported with its subject: %q", got)
	}
}

// TestRetirementSparesAFileInsideTheAge the age errs long on purpose; a live
// session's spool must never be a candidate.
func TestRetirementSparesAFileInsideTheAge(t *testing.T) {
	e := testEngine(t)
	spoolEvent(t, e, "current", "new-1")

	n, err := e.Retire(context.Background())
	if err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if n != 0 {
		t.Errorf("retired %d files, want 0", n)
	}
	if left := spoolLines(t, e.Spool.SessionPath("current")); left != 1 {
		t.Error("a fresh spool file was retired")
	}
}

// TestSweepAndSessionFlushDoNotDoubleDeliver the locking exists; the risk is
// assuming it covers a case it does not. A periodic sweep adds a background
// writer to a directory the hook path also writes.
func TestSweepAndSessionFlushDoNotDoubleDeliver(t *testing.T) {
	e := testEngine(t)
	const session = "sess-race"
	for i := range 40 {
		spoolEvent(t, e, session, fmt.Sprintf("ev-%d", i))
	}

	sink := &deliverySink{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = e.Flush(context.Background(), session, sink) }()
	go func() { defer wg.Done(); _, _ = e.FlushAll(context.Background(), sink) }()
	wg.Wait()

	seen := map[string]int{}
	for _, id := range sink.delivered() {
		seen[id]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("event %s was delivered %d times; the sweep and the session flush overlapped", id, n)
		}
	}
	if len(seen) != 40 {
		t.Errorf("delivered %d distinct events, want 40", len(seen))
	}
}

// spoolLines counts the events left in a spool file; an absent file is empty.
func spoolLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return len(NonEmptyLines(data))
}

// TestADiscardIsReportedEvenWhenTheLogRolledOver the discard log is size-capped
// and RESTARTS rather than being trimmed, so a discard recorded at the rollover
// leaves the log SHORTER than the snapshot taken before the flush. A naive
// before/after length comparison reads that as "nothing new" and swallows exactly
// the discard this reporting exists for.
func TestADiscardIsReportedEvenWhenTheLogRolledOver(t *testing.T) {
	e := testEngine(t)
	var logged strings.Builder
	e.Log = func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) }

	if err := os.MkdirAll(e.Spool.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A log already past its cap: the next write wipes and restarts it.
	fat := make([]byte, maxDiscardLogBytes+1024)
	for i := range fat {
		fat[i] = 'x'
	}
	if err := os.WriteFile(e.Spool.DiscardPath(), fat, 0o600); err != nil {
		t.Fatal(err)
	}

	stem := filepath.Join(e.Spool.Dir, "sess-roll.rec"+fmt.Sprint(MaxRecoveryAttempts)+"-ABCDEF")
	if err := os.WriteFile(stem+".jsonl",
		[]byte(`{"event_id":"doomed","openbox_session_id":"sess-roll"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := e.FlushAll(context.Background(), &refusingSink{}); err == nil {
		t.Log("FlushAll reported no error; the discard is what matters")
	}

	if got := logged.String(); !strings.Contains(got, "DISCARDED") {
		t.Errorf("a discard at the log's rollover was swallowed: %q", got)
	}
}
