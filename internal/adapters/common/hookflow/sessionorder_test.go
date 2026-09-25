package hookflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
)

// countingEmitter is a hookflow.Emitter that records every delivered event id
// in order and answers per-call outcomes from a script; unscripted calls
// succeed. Safe for concurrent use (the concurrent-drainers test below drains
// from two goroutines).
type countingEmitter struct {
	mu       sync.Mutex
	got      []string
	script   map[string]error // event id -> error to return once
	attempts map[string]int   // event id -> number of times Emit was called
}

func (e *countingEmitter) Emit(_ context.Context, ev client.DevEvent) (client.Evaluation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.attempts == nil {
		e.attempts = map[string]int{}
	}
	e.attempts[ev.EventID]++
	e.got = append(e.got, ev.EventID)
	if err, ok := e.script[ev.EventID]; ok {
		return client.Evaluation{}, err
	}
	return client.Evaluation{}, nil
}

func (e *countingEmitter) delivered() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.got...)
}

func (e *countingEmitter) attemptsFor(id string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.attempts[id]
}

// asFlushFunc adapts an Emitter to the FlushFunc Spool.DrainSession takes
// directly (Engine.DrainSession does the same adaptation, via emitFunc, for
// every production caller); these tests exercise Spool.DrainSession itself so
// they can also set Spool.OnFailure and inspect the spool's own files.
func asFlushFunc(em Emitter) FlushFunc {
	return func(ctx context.Context, ev client.DevEvent) error {
		_, err := em.Emit(ctx, ev)
		return err
	}
}

func sessEv(session, id string) client.DevEvent {
	return client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       id,
		EventType:     client.EventToolCall,
		SessionID:     session,
		DeveloperDID:  testDID,
		Timestamp:     "2026-09-24T00:00:00Z",
	}
}

// Delivery order must equal append order across head + tail.
func TestSessionOrder_HeadBeforeTail(t *testing.T) {
	e := &Engine{Spool: Spool{Dir: t.TempDir()}}
	// A head file left behind by an earlier cut pass...
	e.Spool.writeHead("sess", [][]byte{mustLine(t, sessEv("sess", "head-1")), mustLine(t, sessEv("sess", "head-2"))})
	// ...and two more events appended to the tail since.
	if err := e.Spool.Append(sessEv("sess", "tail-1")); err != nil {
		t.Fatal(err)
	}
	if err := e.Spool.Append(sessEv("sess", "tail-2")); err != nil {
		t.Fatal(err)
	}

	em := &countingEmitter{}
	n, err := e.DrainSession(context.Background(), "sess", em, DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout})
	if err != nil {
		t.Fatalf("DrainSession: %v", err)
	}
	if n != 4 {
		t.Fatalf("delivered %d, want 4", n)
	}
	want := []string{"head-1", "head-2", "tail-1", "tail-2"}
	got := em.delivered()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("delivery order = %v, want %v (head must precede tail)", got, want)
	}
}

func mustLine(t *testing.T, ev client.DevEvent) []byte {
	t.Helper()
	l, err := jsonLine(ev)
	if err != nil {
		t.Fatal(err)
	}
	return l[:len(l)-1] // writeHead appends its own newline
}

// Two drainers plus concurrent appends, 200 iterations under -race: every
// event_id is sent exactly once, in order, never duplicated, never reordered
// relative to another event of the SAME session.
func TestSessionOrder_ConcurrentDrainersNeverDuplicateOrReorder(t *testing.T) {
	const iterations = 200
	for iter := range iterations {
		dir := t.TempDir()
		sp := Spool{Dir: dir}
		const perSession = 12
		var want []string
		for i := range perSession {
			id := fmt.Sprintf("i%d-e%d", iter, i)
			if err := sp.Append(sessEv("sess", id)); err != nil {
				t.Fatalf("iter %d: append: %v", iter, err)
			}
			want = append(want, id)
		}

		var mu sync.Mutex
		var got []string
		seen := map[string]int{}
		fn := func(_ context.Context, ev client.DevEvent) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, ev.EventID)
			seen[ev.EventID]++
			return nil
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = sp.DrainSession(context.Background(), "sess", fn, DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout})
		}()
		go func() {
			defer wg.Done()
			_, _ = sp.DrainSession(context.Background(), "sess", fn, DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout})
		}()
		wg.Wait()

		// A residual pass: whichever drainer rotated an empty tail second gets
		// nothing, so at most one of the two above actually delivers anything;
		// this only guards against a stray leftover file.
		_, _ = sp.DrainSession(context.Background(), "sess", fn, DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout})

		for id, n := range seen {
			if n > 1 {
				t.Fatalf("iter %d: event %s delivered %d times (duplicate)", iter, id, n)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("iter %d: delivered %d events, want %d: %v", iter, len(got), len(want), got)
		}
		for i, id := range want {
			if got[i] != id {
				t.Fatalf("iter %d: reordered: got %v, want %v", iter, got, want)
			}
		}
	}
}

// A transient failure gets one attempt and exactly one retry; OnFailure is
// called once per failed event, and the NEXT line still gets attempted (a
// failure never stops the drain).
func TestSessionOrder_OneRetryPerTransientFailure(t *testing.T) {
	classes := []struct {
		name string
		err  error
	}{
		{"network", fmt.Errorf("%w: dial tcp: connection refused", client.ErrDelivery)},
		{"timeout", fmt.Errorf("%w: %w", client.ErrDelivery, context.DeadlineExceeded)},
	}
	for _, tc := range classes {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sp := Spool{Dir: dir}
			if err := sp.Append(sessEv("sess", "failing")); err != nil {
				t.Fatal(err)
			}
			if err := sp.Append(sessEv("sess", "next")); err != nil {
				t.Fatal(err)
			}

			var failed []string
			sp.OnFailure = func(ev client.DevEvent, err error) {
				failed = append(failed, ev.EventID)
				if err != tc.err {
					t.Errorf("OnFailure err = %v, want the classified error", err)
				}
			}

			em := &countingEmitter{script: map[string]error{"failing": tc.err}}
			n, err := sp.DrainSession(context.Background(), "sess", asFlushFunc(em), DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout})
			if err != nil {
				t.Fatalf("DrainSession: %v", err)
			}
			if n != 1 {
				t.Errorf("delivered = %d, want 1 (only the successful line)", n)
			}
			if got := em.attemptsFor("failing"); got != 2 {
				t.Errorf("attempts for the failing line = %d, want exactly 2 (one attempt, one retry)", got)
			}
			if len(failed) != 1 || failed[0] != "failing" {
				t.Errorf("OnFailure called for %v, want exactly [failing]", failed)
			}
			if got := em.delivered(); len(got) != 3 || got[2] != "next" {
				t.Errorf("the line after a failure must still be attempted, got %v", got)
			}
		})
	}
}

// A pass that cannot cover even one attempt from the start touches nothing
// at all -- no rotation, no head file, the tail left exactly as it was -- and
// marks nothing failed: a cutoff is not a failure. Rotating anyway (as an
// earlier version of this code did) would have reset the tail's mtime on a
// pass that attempted nothing, which fools retirement into thinking a
// starving stem just got its shot.
func TestSessionOrder_PreCutoffTouchesNothing(t *testing.T) {
	sp := Spool{Dir: t.TempDir()}
	if err := sp.Append(sessEv("sess", "e1")); err != nil {
		t.Fatal(err)
	}
	tailInfo, err := os.Stat(sp.SessionPath("sess"))
	if err != nil {
		t.Fatal(err)
	}

	failed := 0
	sp.OnFailure = func(client.DevEvent, error) { failed++ }

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	n, err := sp.DrainSession(ctx, "sess", asFlushFunc(&countingEmitter{}), DrainOptions{Mode: Block, AttemptTimeout: time.Minute})
	if n != 0 {
		t.Errorf("delivered = %d, want 0 (the budget could not cover one attempt)", n)
	}
	if !errors.Is(err, errPassCutShort) {
		t.Errorf("err = %v, want errPassCutShort", err)
	}
	if failed != 0 {
		t.Errorf("OnFailure called %d times; a cutoff must never be scored as a failure", failed)
	}
	if _, statErr := os.Stat(filepath.Join(sp.Dir, "sess"+HeadSuffix)); !os.IsNotExist(statErr) {
		t.Errorf("a pre-check cutoff must not create a head file, stat err=%v", statErr)
	}
	afterInfo, err := os.Stat(sp.SessionPath("sess"))
	if err != nil {
		t.Fatalf("the tail itself is gone: %v", err)
	}
	if !afterInfo.ModTime().Equal(tailInfo.ModTime()) {
		t.Error("the tail's mtime changed on a pass that touched nothing; retirement would misread this stem as attempted")
	}
}

// An orphan (a `.flushing.` rotation aged past ReclaimOrphanAfter) is
// discarded, ledgered, and never redelivered.
func TestSessionOrder_OrphanDiscardedAndLedgered(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	orphan := filepath.Join(dir, "sess.jsonl.flushing.dead")
	if err := os.WriteFile(orphan, mustLine(t, sessEv("sess", "orphaned")), 0o600); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-2 * ReclaimOrphanAfter)
	if err := os.Chtimes(orphan, aged, aged); err != nil {
		t.Fatal(err)
	}

	em := &countingEmitter{}
	n, err := sp.DrainSession(context.Background(), "sess", asFlushFunc(em), DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout})
	if err != nil {
		t.Fatalf("DrainSession: %v", err)
	}
	if n != 0 || len(em.delivered()) != 0 {
		t.Fatalf("an orphan must never be redelivered, got n=%d delivered=%v", n, em.delivered())
	}
	if _, statErr := os.Stat(orphan); !os.IsNotExist(statErr) {
		t.Errorf("orphan file should be gone, stat err=%v", statErr)
	}
	if got := sp.DiscardedCount(); got != 1 {
		t.Errorf("DiscardedCount = %d, want 1", got)
	}
}

// A legacy `.rec1-x.jsonl` file (an old binary's carry-over) is discarded
// with one ledger line, never drained.
func TestSessionOrder_LegacyRecoveryFileDiscarded(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	legacy := filepath.Join(dir, "sess.rec1-x.jsonl")
	if err := os.WriteFile(legacy, mustLine(t, sessEv("sess", "legacy")), 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := sp.DrainSession(context.Background(), "sess",
		func(context.Context, client.DevEvent) error {
			t.Error("a legacy carry-over file must never reach the emitter")
			return nil
		}, DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout})
	if err != nil || n != 0 {
		t.Fatalf("DrainSession = (%d, %v), want (0, nil)", n, err)
	}
	if _, statErr := os.Stat(legacy); !os.IsNotExist(statErr) {
		t.Errorf("legacy file should be gone, stat err=%v", statErr)
	}
	if got := sp.DiscardedCount(); got != 1 {
		t.Errorf("DiscardedCount = %d, want 1", got)
	}
}

// Try on a busy stripe reports ErrSessionBusy and sends nothing.
func TestSessionOrder_TryOnBusyStripeReportsBusy(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	if err := sp.Append(sessEv("sess", "e1")); err != nil {
		t.Fatal(err)
	}

	release, err := sp.lockSession(context.Background(), "sess", Try, 0)
	if err != nil {
		t.Fatalf("lockSession: %v", err)
	}
	defer release()

	em := &countingEmitter{}
	n, drainErr := sp.DrainSession(context.Background(), "sess", asFlushFunc(em), DrainOptions{Mode: Try, AttemptTimeout: DeliveryAttemptTimeout})
	if !errors.Is(drainErr, ErrSessionBusy) {
		t.Fatalf("err = %v, want ErrSessionBusy", drainErr)
	}
	if n != 0 || len(em.delivered()) != 0 {
		t.Fatalf("a busy Try must send nothing, got n=%d delivered=%v", n, em.delivered())
	}
}

// A client built with MaxRetries 0 (the same setting claude-code/codex
// creds.go use) never retries at the HTTP layer, so what core sees is the
// drainer's own rule: a 5xx is sent once and retried once, while 401, 429
// and any other 4xx are sent exactly once.
func TestSessionOrder_CoreSeesOneRetryOnlyForTransientStatuses(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   int
	}{{503, 2}, {520, 2}, {401, 1}, {429, 1}, {400, 1}} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			fc := fakecore.New(t, fakecore.Script{AlwaysStatus: tc.status})
			zero := 0
			cl, err := client.New(client.Config{
				BaseURL:            fc.URL(),
				APIKey:             fakecore.APIKey(),
				WorkloadPrivateKey: fakecore.WorkloadPrivateKey(),
				MaxRetries:         &zero,
			})
			if err != nil {
				t.Fatalf("client.New: %v", err)
			}

			sp := Spool{Dir: t.TempDir()}
			if err := sp.Append(sessEv("sess", "e1")); err != nil {
				t.Fatal(err)
			}
			var failed int
			sp.OnFailure = func(client.DevEvent, error) { failed++ }

			if _, err := sp.DrainSession(context.Background(), "sess", asFlushFunc(cl), DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout}); err != nil {
				t.Fatalf("DrainSession: %v", err)
			}
			if got := fc.V3EvaluateAttempts(); got != tc.want {
				t.Errorf("core saw %d request(s), want exactly %d", got, tc.want)
			}
			if failed != 1 {
				t.Errorf("OnFailure called %d times, want 1", failed)
			}
		})
	}
}

// --- Inline SessionStart/SessionEnd delivery tests ---
//
// These pin the mechanism claude-code/codex's own inlineAttempt helper is
// built on (append the session's own event, then DrainSession(Block, a small
// window) for that SAME session before returning from the hook): this
// package has no subprocess to spawn a flusher from, so the adapter-level
// "falls back to the flusher" behaviour is exercised here as a cutoff that a
// LATER, unbounded drain completes -- the flusher's own job once spawned --
// rather than by actually forking a child process.

// workflowStarted returns a SessionStarted event, this repo's shape for core's
// own "WorkflowStarted" activity row.
func workflowStarted(session, id string) client.DevEvent {
	ev := sessEv(session, id)
	ev.EventType = client.EventSessionStarted
	return ev
}

// Fresh session, realtime flush OFF, no flusher ever spawned: the first row
// core sees is WorkflowStarted, even racing a concurrent PreToolUse append of
// the same session (in-process, the way a hook and a slightly-later tool call
// would).
func TestSessionOrder_InlineStartIsFirstEvenUnderRace(t *testing.T) {
	sp := Spool{Dir: t.TempDir()}
	if err := sp.Append(workflowStarted("sess", "start")); err != nil {
		t.Fatal(err)
	}

	em := &countingEmitter{}
	raced := make(chan struct{})
	go func() {
		defer close(raced)
		_ = sp.Append(sessEv("sess", "racing-tool-call"))
	}()

	// The inline attempt: append (above), then drain under the SAME session's
	// stripe, before anything else of this run can be sent -- exactly what
	// claude-code/codex's inlineAttempt does at SessionStart.
	if _, err := sp.DrainSession(context.Background(), "sess", asFlushFunc(em), DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout}); err != nil {
		t.Fatalf("DrainSession: %v", err)
	}
	<-raced

	// Whatever this pass delivered, the start must be first; the race either
	// lost (its append landed after the rotation, so this pass never saw it)
	// or won (it landed before, so it still queued strictly after the start
	// already in the file) -- head/append order makes both outcomes safe.
	got := em.delivered()
	if len(got) == 0 || got[0] != "start" {
		t.Fatalf("first delivered event = %v, want \"start\" first", got)
	}

	// Whatever the race left queued reaches core on the next (unbounded)
	// drain, still after the start.
	fn2, got2 := drainCollect()
	if _, err := sp.DrainSession(context.Background(), "sess", fn2, DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout}); err != nil {
		t.Fatalf("second DrainSession: %v", err)
	}
	if len(*got2) > 0 && (*got2)[0].EventID == "start" {
		t.Errorf("the start was redelivered on the next pass: %v", eventIDs(*got2))
	}
}

func eventIDs(evs []client.DevEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.EventID
	}
	return out
}

// A SessionStart that core rejects: OnFailure fires once (the ledger line),
// after one retry for a 5xx and none for a 401; the run-halt latch itself is
// a later caller's own OnFailure, wired onto this same seam.
func TestSessionOrder_RejectedStartCallsOnFailureOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   int
	}{{"5xx", 503, 2}, {"401", 401, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			fc := fakecore.New(t, fakecore.Script{AlwaysStatus: tc.status})
			zero := 0
			cl, err := client.New(client.Config{
				BaseURL:            fc.URL(),
				APIKey:             fakecore.APIKey(),
				WorkloadPrivateKey: fakecore.WorkloadPrivateKey(),
				MaxRetries:         &zero,
			})
			if err != nil {
				t.Fatalf("client.New: %v", err)
			}
			sp := Spool{Dir: t.TempDir()}
			if err := sp.Append(workflowStarted("sess", "start")); err != nil {
				t.Fatal(err)
			}
			var failed []string
			sp.OnFailure = func(ev client.DevEvent, err error) {
				failed = append(failed, ev.EventID)
				if !errors.Is(err, client.ErrDelivery) {
					t.Errorf("OnFailure err = %v, want it to carry ErrDelivery", err)
				}
			}
			if _, err := sp.DrainSession(context.Background(), "sess", asFlushFunc(cl), DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout}); err != nil {
				t.Fatalf("DrainSession: %v", err)
			}
			if len(failed) != 1 || failed[0] != "start" {
				t.Fatalf("OnFailure calls = %v, want exactly one for \"start\"", failed)
			}
			if got := fc.V3EvaluateAttempts(); got != tc.want {
				t.Errorf("core saw %d attempt(s) for the rejected start, want %d", got, tc.want)
			}
			if got := sp.PendingCount("sess"); got != 0 {
				t.Errorf("PendingCount = %d, want 0: a rejected start is attempted-and-gone, not requeued", got)
			}
		})
	}
}

// A SessionStart against a core slower than the hook window: the inline
// attempt cannot even start (not a failure), so no halt is triggered; the
// start is left for the next (unbounded) drain -- the flusher's role once
// spawned -- and it is STILL the first row of the run, because nothing else
// of the run was ever attempted ahead of it.
func TestSessionOrder_SlowCoreDefersNotFails(t *testing.T) {
	sp := Spool{Dir: t.TempDir()}
	if err := sp.Append(workflowStarted("sess", "start")); err != nil {
		t.Fatal(err)
	}

	failed := 0
	sp.OnFailure = func(client.DevEvent, error) { failed++ }

	// The hook's own window has already nearly elapsed by the time it tries to
	// drain (a slow machine, a cold process start): remaining is under
	// attemptTimeout, so DrainSession must not even start the attempt.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	em := &countingEmitter{}
	if _, err := sp.DrainSession(ctx, "sess", asFlushFunc(em), DrainOptions{Mode: Block, AttemptTimeout: time.Minute}); err == nil {
		t.Error("want a cutoff error")
	}
	if failed != 0 {
		t.Errorf("a deferred attempt must never be scored as a failure (no halt), got %d OnFailure call(s)", failed)
	}
	if len(em.delivered()) != 0 {
		t.Fatalf("core saw a delivery before the window even allowed one: %v", em.delivered())
	}

	// Meanwhile the rest of the run appends normally (the tool call the
	// developer made right after the slow SessionStart hook returned).
	if err := sp.Append(sessEv("sess", "first-tool-call")); err != nil {
		t.Fatal(err)
	}

	// The flusher's own unbounded pass: core keeps exactly one copy of the
	// start, and it is still first.
	fn, got := drainCollect()
	if _, err := sp.DrainSession(context.Background(), "sess", fn, DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout}); err != nil {
		t.Fatalf("flusher drain: %v", err)
	}
	if len(*got) != 2 || (*got)[0].EventID != "start" || (*got)[1].EventID != "first-tool-call" {
		t.Fatalf("delivered %v, want [start first-tool-call] in that order", eventIDs(*got))
	}
}

// --- Crash-safety, requeue-on-unanswered, and cut-pass churn tests ---

// A process killed between collectSession's read and DrainSession's own
// cleanup (Codex SIGKILLs a SessionEnd hook at 3s) must not lose the lines
// it had already read into memory: they are still sitting, untouched, in
// the rotated `.flushing.` file collectSession left behind, so the next
// drainer's orphan reclaim finds them, ledgers them, and fails them
// individually -- never silently, and never redelivered.
func TestSessionOrder_KilledMidPassLeavesAReclaimableOrphan(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	if err := sp.Append(sessEv("sess", "e1")); err != nil {
		t.Fatal(err)
	}
	if err := sp.Append(sessEv("sess", "e2")); err != nil {
		t.Fatal(err)
	}

	// Exactly what a real DrainSession call does up through the read -- and
	// exactly where it stops if the process dies right here: collectSession
	// itself no longer removes what it read (that decision now belongs to
	// DrainSession, made only once the remainder is durably queued).
	lines, rotated, err := sp.collectSession("sess")
	if err != nil {
		t.Fatalf("collectSession: %v", err)
	}
	if len(lines) != 2 || len(rotated) == 0 {
		t.Fatalf("precondition: want 2 lines and at least one rotated file, got %d lines, rotated=%v", len(lines), rotated)
	}
	for _, p := range rotated {
		if _, statErr := os.Stat(p); statErr != nil {
			t.Fatalf("precondition: rotated file missing before the simulated crash: %v", statErr)
		}
	}

	// The simulated crash: nothing else runs. Neither line was attempted,
	// neither was queued to a head file, and the rotated file was never
	// removed -- exactly the on-disk state a kill leaves.
	for _, p := range rotated {
		aged := time.Now().Add(-2 * ReclaimOrphanAfter)
		if err := os.Chtimes(p, aged, aged); err != nil {
			t.Fatal(err)
		}
	}

	// The next drainer for this session (the sweep, or the developer's next
	// session) must reclaim it: ledgered, failed individually, never sent.
	var failed []string
	sp.OnFailure = func(ev client.DevEvent, err error) {
		failed = append(failed, ev.EventID)
		sp.defaultOnFailure(ev, err)
	}
	em := &countingEmitter{}
	n, err := sp.DrainSession(context.Background(), "sess", asFlushFunc(em), DrainOptions{Mode: Block, AttemptTimeout: DeliveryAttemptTimeout})
	if err != nil {
		t.Fatalf("DrainSession: %v", err)
	}
	if n != 0 || len(em.delivered()) != 0 {
		t.Fatalf("the killed pass's lines were redelivered: n=%d delivered=%v", n, em.delivered())
	}
	if len(failed) != 2 || failed[0] != "e1" || failed[1] != "e2" {
		t.Fatalf("OnFailure calls = %v, want exactly [e1 e2]", failed)
	}
	if got := sp.DiscardedCount(); got != 2 {
		t.Errorf("DiscardedCount = %d, want 2", got)
	}
	for _, p := range rotated {
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Errorf("the orphan should be consumed, stat err=%v", statErr)
		}
	}
}

// blockingEmitter blocks Emit until its ctx is Done, then answers with err
// (default: a generic failure) -- the shape of a core that never responds
// within an attempt's own window.
type blockingEmitter struct{ err error }

func (b blockingEmitter) Emit(ctx context.Context, _ client.DevEvent) (client.Evaluation, error) {
	<-ctx.Done()
	if b.err != nil {
		return client.Evaluation{}, b.err
	}
	return client.Evaluation{}, fmt.Errorf("%w: core never answered", client.ErrDelivery)
}

// With RequeueUnanswered, an attempt whose own ctx expired before core
// answered is requeued, not failed: the line (and everything after it) goes
// to the head file, OnFailure never runs for it, and the next line is never
// attempted (its predecessor's outcome is still unknown, so nothing may
// overtake it).
func TestSessionOrder_UnansweredIsRequeuedNotFailed(t *testing.T) {
	sp := Spool{Dir: t.TempDir()}
	if err := sp.Append(sessEv("sess", "unanswered")); err != nil {
		t.Fatal(err)
	}
	if err := sp.Append(sessEv("sess", "next")); err != nil {
		t.Fatal(err)
	}

	var failed []string
	sp.OnFailure = func(ev client.DevEvent, _ error) { failed = append(failed, ev.EventID) }

	n, err := sp.DrainSession(context.Background(), "sess", asFlushFunc(blockingEmitter{}),
		DrainOptions{Mode: Block, AttemptTimeout: 20 * time.Millisecond, RequeueUnanswered: true})
	if n != 0 {
		t.Errorf("delivered = %d, want 0", n)
	}
	if err == nil {
		t.Error("want a cutoff error")
	}
	if len(failed) != 0 {
		t.Errorf("OnFailure called for %v; an unanswered attempt must be requeued, not failed", failed)
	}
	if got := sp.PendingCount("sess"); got != 2 {
		t.Errorf("PendingCount = %d, want 2 (the unanswered line AND the one after it, never attempted)", got)
	}
	data, readErr := os.ReadFile(filepath.Join(sp.Dir, "sess"+HeadSuffix))
	if readErr != nil {
		t.Fatalf("head file not written: %v", readErr)
	}
	if got := len(NonEmptyLines(data)); got != 2 {
		t.Errorf("head file holds %d line(s), want 2", got)
	}
}

// Without RequeueUnanswered (the 30s flusher/sweeper/uninstall default), the
// exact same silence is scored as an ordinary failure, matching the
// pre-existing behaviour those callers rely on.
func TestSessionOrder_UnansweredIsFailedWhenNotRequeued(t *testing.T) {
	sp := Spool{Dir: t.TempDir()}
	if err := sp.Append(sessEv("sess", "unanswered")); err != nil {
		t.Fatal(err)
	}

	var failed []string
	sp.OnFailure = func(ev client.DevEvent, _ error) { failed = append(failed, ev.EventID) }

	n, err := sp.DrainSession(context.Background(), "sess", asFlushFunc(blockingEmitter{}),
		DrainOptions{Mode: Block, AttemptTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("DrainSession: %v", err)
	}
	if n != 0 {
		t.Errorf("delivered = %d, want 0", n)
	}
	if len(failed) != 1 || failed[0] != "unanswered" {
		t.Fatalf("OnFailure calls = %v, want exactly [unanswered]", failed)
	}
	if got := sp.PendingCount("sess"); got != 0 {
		t.Errorf("PendingCount = %d, want 0: without RequeueUnanswered this is an ordinary, consumed failure", got)
	}
}

// FlushAll must stop its walk, not merely record the error, once one stem
// reports errPassCutShort: every stem still to come shares the same ctx and
// would see the identical remaining budget, so touching each of them only to
// reject it instantly would churn their mtimes for nothing.
func TestSessionOrder_FlushAllStopsOnCutShort(t *testing.T) {
	sp := Spool{Dir: t.TempDir()}
	if err := sp.Append(sessEv("aaa-first", "a1")); err != nil {
		t.Fatal(err)
	}
	if err := sp.Append(sessEv("zzz-second", "z1")); err != nil {
		t.Fatal(err)
	}
	before := map[string]time.Time{}
	for _, id := range []string{"aaa-first", "zzz-second"} {
		info, statErr := os.Stat(sp.SessionPath(id))
		if statErr != nil {
			t.Fatal(statErr)
		}
		before[id] = info.ModTime()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	n, err := sp.FlushAll(ctx, asFlushFunc(&countingEmitter{}))
	if n != 0 {
		t.Errorf("delivered = %d, want 0", n)
	}
	if !errors.Is(err, errPassCutShort) {
		t.Fatalf("err = %v, want errPassCutShort", err)
	}
	for _, id := range []string{"aaa-first", "zzz-second"} {
		info, statErr := os.Stat(sp.SessionPath(id))
		if statErr != nil {
			t.Fatalf("%s: tail file disappeared: %v", id, statErr)
		}
		if !info.ModTime().Equal(before[id]) {
			t.Errorf("%s: mtime changed on a pass that could not cover one attempt anywhere", id)
		}
	}
}

// FlushOrSweep must treat errPassCutShort exactly like ctx.Err(): a stem
// DrainSession refused to even rotate is exactly as untried as one a walk
// never reached, so retirement must not run.
func TestSessionOrder_FlushOrSweepSkipsRetirementOnCutShort(t *testing.T) {
	e := &Engine{Spool: Spool{Dir: t.TempDir()}}
	var logged strings.Builder
	e.Log = func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) }

	if err := e.Spool.Append(sessEv("ancient", "old-1")); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-RetireSpoolAfter - time.Hour)
	if err := os.Chtimes(e.Spool.SessionPath("ancient"), old, old); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := e.FlushOrSweep(ctx, "", &countingEmitter{}); !errors.Is(err, errPassCutShort) {
		t.Fatalf("FlushOrSweep err = %v, want errPassCutShort", err)
	}
	if _, statErr := os.Stat(e.Spool.SessionPath("ancient")); statErr != nil {
		t.Errorf("a file this pass could not even touch was retired: %v", statErr)
	}
	if !strings.Contains(logged.String(), "skipping retirement") {
		t.Errorf("the skip was not reported: %q", logged.String())
	}
}
