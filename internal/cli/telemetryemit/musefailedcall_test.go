package telemetryemit

import (
	"context"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// eof_clean is the one completion signal Muse's model_call carries (1 on every
// call of the scrubbed export). A call whose stream did not end cleanly is
// partial: the contract has no failed status for a turn's close, so it is
// emitted as the pair it is, marked, rather than passed off as a clean turn.
func TestMusePartialCallIsMarkedNotPassedOffAsClean(t *testing.T) {
	for _, falsy := range []string{"0", "false"} {
		events, out := museMapper().EventsFor(museModelCall(map[string]string{"eof_clean": falsy}))
		if out != Emitted || len(events) != 2 {
			t.Fatalf("eof_clean=%s: outcome %v with %d events, want the Started/Completed pair", falsy, out, len(events))
		}
		for _, ev := range events {
			_, marked := ev.Metadata["response_incomplete"]
			if want := ev.EventType == client.EventTurnCompleted; marked != want {
				t.Errorf("eof_clean=%s %s: response_incomplete present=%v, want %v (known only at the close)", falsy, ev.EventType, marked, want)
			}
		}
	}
	// Clean, or silent about it: unmarked, as before.
	for name, o := range map[string]map[string]string{"clean": nil, "absent": {"eof_clean": ""}} {
		events, out := museMapper().EventsFor(museModelCall(o))
		if out != Emitted {
			t.Fatalf("%s: outcome %v", name, out)
		}
		for _, ev := range events {
			if _, marked := ev.Metadata["response_incomplete"]; marked {
				t.Errorf("%s: %s is marked incomplete", name, ev.EventType)
			}
		}
	}
}

// A failed call usually never got a response id. That is the call failing, not
// the lane losing a record, so it is a skip: no drop, no loss warning.
func TestMuseFailedCallWithNoResponseIDIsASkipNotALoss(t *testing.T) {
	rec := museModelCall(map[string]string{"eof_clean": "0", "gen_ai_response_id": ""})
	events, out := museMapper().EventsFor(rec)
	if out != SkipIncompleteCall || len(events) != 0 || out.IsDrop() {
		t.Fatalf("outcome %v (drop=%v) with %d events, want a non-drop SkipIncompleteCall", out, out.IsDrop(), len(events))
	}
	// A clean call with no id is still a loss.
	if _, out := museMapper().EventsFor(museModelCall(map[string]string{"gen_ai_response_id": ""})); out != DropNoRequestID {
		t.Errorf("clean call with no id: outcome %v, want DropNoRequestID", out)
	}

	var warned int
	em := &Emitter{
		Mapper:  museMapper(),
		DID:     func() string { return testDID },
		Warn:    func(string, ...any) { warned++ },
		Deliver: func(context.Context, client.DevEvent) bool { return true },
	}
	_ = em.Emit(context.Background(), rec)
	if warned != 0 {
		t.Errorf("a failed call with no response id raised the loss warning %d time(s)", warned)
	}
	if _, drops := em.Stats(); drops[SkipIncompleteCall.String()] != 1 || drops[DropNoRequestID.String()] != 0 {
		t.Errorf("counters = %v, want one %s and no %s", drops, SkipIncompleteCall, DropNoRequestID)
	}
	if got := traceOutcomeName(SkipIncompleteCall); got != "skipped" {
		t.Errorf("trace outcome = %q, want skipped", got)
	}
}
