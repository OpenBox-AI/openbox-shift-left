package hookflow

import (
	"context"
	"errors"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// verdictEmitter is a minimal Emitter returning a fixed verdict/error, for
// pinning Deliver's latch condition in isolation from any real transport.
type verdictEmitter struct {
	eval client.Evaluation
	err  error
}

func (v verdictEmitter) Emit(context.Context, client.DevEvent) (client.Evaluation, error) {
	return v.eval, v.err
}

// TestDeliverLatchesOnHaltVerdict is the load-bearing case: err == nil &&
// Verdict == HALT writes the latch, keyed on the event's own session id when
// it carries no run id (generation 0).
func TestDeliverLatchesOnHaltVerdict(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	ev := client.DevEvent{EventID: "e1", SessionID: "sess-halt-1"}
	em := verdictEmitter{eval: client.Evaluation{Verdict: client.VerdictHalt, Reason: "policy says stop", PolicyID: "p-9"}}

	if _, err := Deliver(context.Background(), em, nil, ev, nopLogger()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	info, halted := SessionHalted("sess-halt-1")
	if !halted {
		t.Fatal("a HALT verdict with no delivery error did not latch the session's run")
	}
	if info.Reason != "policy says stop" || info.PolicyID != "p-9" {
		t.Errorf("latch info = %+v, want the delivered verdict's reason and policy id", info)
	}
}

// TestDeliverLatchesOnTheRunIDWhenOneWasStamped: a record whose producer
// already resolved and stamped RunID (a continued run) latches THAT run, not
// its session id -- the correction the brief itself got wrong once.
func TestDeliverLatchesOnTheRunIDWhenOneWasStamped(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	ev := client.DevEvent{EventID: "e1", SessionID: "sess-continued", RunID: "run-xyz"}
	em := verdictEmitter{eval: client.Evaluation{Verdict: client.VerdictHalt, Reason: "stop"}}

	if _, err := Deliver(context.Background(), em, nil, ev, nopLogger()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	if _, halted := SessionHalted("run-xyz"); !halted {
		t.Error("a record carrying RunID must latch the RUN, not the session id")
	}
	if _, halted := SessionHalted("sess-continued"); halted {
		t.Error("the session id itself must not read as halted; only the run it currently belongs to")
	}
}

// TestDeliverDoesNotLatchOnANonHaltVerdict covers every other verdict:
// nothing but HALT ever writes the latch.
func TestDeliverDoesNotLatchOnANonHaltVerdict(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	for _, v := range []client.Verdict{client.VerdictAllow, client.VerdictConstrain, client.VerdictRequireApproval, client.VerdictBlock} {
		ev := client.DevEvent{EventID: "e1", SessionID: "sess-" + string(v)}
		em := verdictEmitter{eval: client.Evaluation{Verdict: v}}
		if _, err := Deliver(context.Background(), em, nil, ev, nopLogger()); err != nil {
			t.Fatalf("Deliver(%s): %v", v, err)
		}
		if _, halted := SessionHalted(ev.SessionID); halted {
			t.Errorf("verdict %s must never latch", v)
		}
	}
}

// TestDeliverDoesNotLatchOnATransportError pins that em.Emit returning an
// error (a control-plane outage, per client.Client.Emit's own contract, which
// returns the zero Evaluation on failure) must never be misread as a HALT --
// even in the deliberately adversarial case where the zero-ish Evaluation
// somehow still carried VerdictHalt, err != nil alone must suppress the latch.
func TestDeliverDoesNotLatchOnATransportError(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	ev := client.DevEvent{EventID: "e1", SessionID: "sess-unreachable"}
	em := verdictEmitter{eval: client.Evaluation{Verdict: client.VerdictHalt}, err: errors.New("control plane unreachable")}

	if _, err := Deliver(context.Background(), em, nil, ev, nopLogger()); err == nil {
		t.Fatal("Deliver swallowed the delivery error")
	}
	if _, halted := SessionHalted("sess-unreachable"); halted {
		t.Error("a delivery error must never latch, even carrying a HALT-shaped Evaluation")
	}
}

// TestDeliverLatchWriteToleratesANilLogger: a caller indifferent to the
// latch's own diagnostics (this test, a pool built without one) must not
// panic three calls deep in WriteSessionHalt, which itself requires a
// non-nil *log.Logger.
func TestDeliverLatchWriteToleratesANilLogger(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	ev := client.DevEvent{EventID: "e1", SessionID: "sess-nil-logger"}
	em := verdictEmitter{eval: client.Evaluation{Verdict: client.VerdictHalt}}

	if _, err := Deliver(context.Background(), em, nil, ev, nil); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if _, halted := SessionHalted("sess-nil-logger"); !halted {
		t.Error("a nil logger must not suppress the latch write")
	}
}

// TestDeliverRecordsAdvisoryAndLatchesTogether: the widened write side does
// not disturb the pre-existing advisory-record contract (TestDeliverRecordsAdvisory).
func TestDeliverRecordsAdvisoryAndLatchesTogether(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	dir := t.TempDir() + "/advisory.jsonl"
	adv := &Advisory{Path: dir}
	ev := client.DevEvent{EventID: "e1", SessionID: "sess-adv"}
	em := verdictEmitter{eval: client.Evaluation{Verdict: client.VerdictHalt}}

	if _, err := Deliver(context.Background(), em, adv, ev, nopLogger()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if _, halted := SessionHalted("sess-adv"); !halted {
		t.Error("supplying an Advisory must not suppress the latch write")
	}
}

// TestHookFlushHaltLatches is the hook-flusher half of the latch contract: a
// spooled event delivered through Engine.Flush (emitFunc's body, the same
// function `openbox hook <provider> flush` runs) latches exactly as the
// in-process pool path does -- one shared delivery body, not two.
func TestHookFlushHaltLatches(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	const session = "sess-hook-flush"
	e := testEngine(t)
	spoolEvent(t, e, session, "e1")

	em := verdictEmitter{eval: client.Evaluation{Verdict: client.VerdictHalt, Reason: "stop"}}
	if _, err := e.Flush(context.Background(), session, em); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if _, halted := SessionHalted(session); !halted {
		t.Error("a HALT delivered through the hook flusher's Flush must latch the run, exactly as an in-process pool delivery does")
	}
}
