package hookflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// carryOverByAttempt reads every carry-over file back through the production
// parser rather than by matching names, so the assertion fails if RecoveryAttempt
// and the writer ever disagree.
func carryOverByAttempt(t *testing.T, dir string) map[int][]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	out := map[int][]string{}
	for _, e := range entries {
		name := e.Name()
		if !IsRecoveryFile(name) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		attempt := RecoveryAttempt(name)
		for _, l := range NonEmptyLines(data) {
			var d client.DevEvent
			if json.Unmarshal(l, &d) != nil {
				t.Fatalf("corrupt line in %s", name)
			}
			out[attempt] = append(out[attempt], d.EventID)
		}
	}
	return out
}

// seedCarryOver writes a carry-over file already at `attempt`. Starting above 1
// matters: writeRecovery clamps next<1 to 1, so a virgin file collapses both
// populations onto .rec1 and the two numbers cannot be told apart.
func seedCarryOver(t *testing.T, dir, session string, attempt int, ids ...string) string {
	t.Helper()
	var buf []byte
	for _, id := range ids {
		l, err := jsonLine(ev(session, id))
		if err != nil {
			t.Fatalf("marshal %s: %v", id, err)
		}
		buf = append(buf, l...)
	}
	path := filepath.Join(dir, fmt.Sprintf("%s.rec%d-SEEDSEEDSEED.jsonl", session, attempt))
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return path
}

// TestARefusedLineAndAnUnreachedLineCarryDifferentAttempts is the split itself.
// One file cannot carry two attempt numbers, and merging the two populations is
// what put the discard bound out of reach.
func TestARefusedLineAndAnUnreachedLineCarryDifferentAttempts(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	seed := seedCarryOver(t, dir, "sess", 2, "refused", "delivered", "unreached")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn := func(_ context.Context, e client.DevEvent) error {
		switch e.EventID {
		case "refused":
			// Only a proven refusal spends an attempt, so the sentinel is required.
			return fmt.Errorf("%w: %w: refused", client.ErrDelivery, client.ErrRefused)
		case "delivered":
			cancel() // the budget dies after a clean delivery; the loop head catches it
			return nil
		}
		t.Errorf("the budget should have ended the drain before %q", e.EventID)
		return nil
	}
	if _, err := sp.drainFile(ctx, seed, fn); err == nil {
		t.Fatal("expected the deadline error")
	}

	got := carryOverByAttempt(t, dir)
	if want := []string{"refused"}; len(got[3]) != 1 || got[3][0] != want[0] {
		t.Errorf("attempt 3 holds %v, want %v: a refusal must advance even when the budget then expired", got[3], want)
	}
	if want := []string{"unreached"}; len(got[2]) != 1 || got[2][0] != want[0] {
		t.Errorf("attempt 2 holds %v, want %v: the budget refused this line nothing", got[2], want)
	}
}

// TestALineTheDeadlineCutMidDeliveryKeepsItsAttempt is the clause the whole fix
// turns on. Emit reports a budget-cut POST as `%w: %s` on ErrDelivery, flattening
// the cause to text, so errors.Is cannot see context.DeadlineExceeded and the
// context is the only witness. Scoring that as a refusal would make the retry cap
// a timer: a control plane that merely hangs would delete a backlog in five
// sweeps, which is exactly what the live flusher log was full of.
func TestALineTheDeadlineCutMidDeliveryKeepsItsAttempt(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	seedCarryOver(t, dir, "sess", 2, "cut-mid-flight")

	for pass := range MaxRecoveryAttempts * 3 {
		ctx, cancel := context.WithCancel(context.Background())
		fn := func(context.Context, client.DevEvent) error {
			cancel() // the budget expires inside the POST, as a spent one does
			return fmt.Errorf("%w: %s", client.ErrDelivery, context.DeadlineExceeded)
		}
		if _, err := sp.FlushAll(ctx, fn); err == nil {
			t.Fatalf("pass %d: expected the deadline error", pass)
		}
		cancel()
	}

	if got := sp.DiscardedCount(); got != 0 {
		t.Errorf("%d event(s) discarded by a budget that refused nothing", got)
	}
	if got := sp.BacklogCount(); got != 1 {
		t.Errorf("backlog = %d, want the event still waiting", got)
	}
	for attempt, ids := range carryOverByAttempt(t, dir) {
		if attempt != 2 {
			t.Errorf("%v advanced to attempt %d; a cut-off delivery refused nothing", ids, attempt)
		}
	}
}

// TestABacklogTooBigForOneBudgetStillReachesTheDiscardBound is the measured
// symptom: a carry-over file sat at .rec1- across dozens of passes for an hour,
// burning a whole budget each time, because the count only advanced when the loop
// reached the end of a file no budget was ever large enough to finish.
func TestABacklogTooBigForOneBudgetStillReachesTheDiscardBound(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	const events = 6
	for i := range events {
		if err := sp.Append(ev("sess", fmt.Sprintf("e%d", i))); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	const perPass = 2 // a budget that cannot cover the file
	const maxPasses = 200
	passes := 0
	for ; passes < maxPasses && sp.BacklogCount() > 0; passes++ {
		ctx, cancel := context.WithCancel(context.Background())
		tried := 0
		fn := func(context.Context, client.DevEvent) error {
			tried++
			if tried > perPass {
				cancel()
				return fmt.Errorf("%w: %s", client.ErrDelivery, context.DeadlineExceeded)
			}
			return fmt.Errorf("%w: %w: refused", client.ErrDelivery, client.ErrRefused)
		}
		_, _ = sp.FlushAll(ctx, fn)
		cancel()
	}

	if got := sp.BacklogCount(); got != 0 {
		t.Fatalf("after %d passes the backlog is still %d; the discard bound is unreachable "+
			"for a file bigger than one budget", passes, got)
	}
	if got := sp.DiscardedCount(); got != events {
		t.Errorf("discarded = %d, want %d", got, events)
	}
}

// TestATransportFaultDoesNotBurnADeliveryAttempt is the policy the owner chose:
// only a proven refusal spends one of an event's five attempts. A laptop offline
// for five sweeps, an exhausted 5xx, or a 401 -- which core also answers when its
// own datastore lookup fails, making a database blip wire-identical to a revoked
// key -- must all leave the count where it was. Otherwise the retry cap is a
// timer, and the spool is the only copy of the evidence it would delete.
func TestATransportFaultDoesNotBurnADeliveryAttempt(t *testing.T) {
	dir := t.TempDir()
	sp := Spool{Dir: dir}
	seedCarryOver(t, dir, "sess", 2, "held")

	for pass := range MaxRecoveryAttempts * 3 {
		fn := func(context.Context, client.DevEvent) error {
			return fmt.Errorf("%w: dial tcp 127.0.0.1:443: connect: connection refused",
				client.ErrDelivery)
		}
		if _, err := sp.FlushAll(context.Background(), fn); err != nil {
			t.Fatalf("pass %d: a held line is not a failed pass: %v", pass, err)
		}
	}

	if got := sp.DiscardedCount(); got != 0 {
		t.Errorf("%d event(s) discarded; nothing ever refused them", got)
	}
	if got := sp.BacklogCount(); got != 1 {
		t.Errorf("backlog = %d, want the event still waiting", got)
	}
	for attempt, ids := range carryOverByAttempt(t, dir) {
		if attempt != 2 {
			t.Errorf("%v advanced to attempt %d; a transport fault judged nothing", ids, attempt)
		}
	}
}
