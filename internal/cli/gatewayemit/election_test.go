package gatewayemit

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// electionEmitter is an emitter whose gate the test controls.
func electionEmitter(t *testing.T, elected func() bool) (*Emitter, *fakeDelivery, *bytes.Buffer) {
	t.Helper()
	delivery := newFakeDelivery()
	var warnings bytes.Buffer
	em := &Emitter{
		Lane:    LaneProxy,
		Deliver: delivery.Deliver,
		DID:     func() string { return testDID },
		Warn:    func(format string, args ...any) { fmt.Fprintf(&warnings, format+"\n", args...) },
		Elected: elected,
		// A NAMED other lane, which is the healthy not-elected state. The unnamed
		// one is a routing gap and has its own test.
		ElectedName: func() string { return string(LaneGateway.Name) },
	}
	return em, delivery, &warnings
}

// TestAnUnelectedLaneEmitsNothing is the mutual exclusion that was unenforced.
// Policy.Elected was read by the telemetry lane alone; the in-path emitters had
// no such field and emitted unconditionally. One lane emitting was the correct
// outcome, reached by two independent faults cancelling each other out.
func TestAnUnelectedLaneEmitsNothing(t *testing.T) {
	em, spool, warnings := electionEmitter(t, func() bool { return false })
	em.Emit(context.Background(), capturedWithSession("sess-1"))

	if n := len(spooledEvents(t, spool, "sess-1")); n != 0 {
		t.Errorf("an unelected lane spooled %d event(s)", n)
	}
	// Quiet, not loud: another lane owning this machine's model calls is the
	// normal healthy state, and warning about it would train a reader to ignore
	// the warnings that matter.
	if got := warnings.String(); got != "" {
		t.Errorf("an unelected lane warned: %q", got)
	}
}

// TestAnElectedLaneEmits is the positive control. Without it the test above
// passes on an emitter that is broken for some entirely different reason.
func TestAnElectedLaneEmits(t *testing.T) {
	em, spool, _ := electionEmitter(t, func() bool { return true })
	em.Emit(context.Background(), capturedWithSession("sess-1"))

	if n := len(spooledEvents(t, spool, "sess-1")); n != 2 {
		t.Errorf("an elected lane spooled %d event(s), want the pair", n)
	}
}

// TestANilElectionGateIsLoud is the failure mode this repair could otherwise
// introduce, and it is the one nobody would see: a lane wired without its gate
// would silence the only working producer on the machine, and a bare `return`
// would make that indistinguishable from a quiet session.
//
// telemetryemit's zero value suppresses silently, because its policy is a struct
// a caller fills in; here the field is on the emitter itself and every production
// caller sets it, so nil is a wiring defect and says so.
func TestANilElectionGateIsLoud(t *testing.T) {
	em, spool, warnings := electionEmitter(t, nil)
	em.Emit(context.Background(), capturedWithSession("sess-1"))

	if n := len(spooledEvents(t, spool, "sess-1")); n != 0 {
		t.Errorf("an emitter with no gate spooled %d event(s); the zero value must suppress", n)
	}
	got := warnings.String()
	if got == "" {
		t.Fatal("an emitter with no election gate said nothing; this is the state where the only " +
			"working lane on the machine goes silent, and silence is what made it invisible")
	}
	if !strings.Contains(got, "wiring defect") {
		t.Errorf("the warning does not distinguish a defect from a setting: %q", got)
	}
}

// TestTheGateIsConsultedPerCall asserts the SECOND call, not the first. Install
// ordering starts a lane daemon before the env var is written, so the first
// answer is legitimately "no" and a gate consulted once would stay that way for
// the process's whole life. This repo has paid for a first-invocation-only test
// before: fifteen green ones missed a defect because each ran init exactly once.
func TestTheGateIsConsultedPerCall(t *testing.T) {
	elected := false
	em, spool, _ := electionEmitter(t, func() bool { return elected })

	em.Emit(context.Background(), capturedWithSession("sess-1"))
	if n := len(spooledEvents(t, spool, "sess-1")); n != 0 {
		t.Fatalf("spooled %d event(s) while unelected", n)
	}

	// The env var lands, exactly as it does moments after install.
	elected = true
	em.Emit(context.Background(), capturedWithSession("sess-1"))

	if n := len(spooledEvents(t, spool, "sess-1")); n != 2 {
		t.Errorf("spooled %d event(s) after the lane became elected, want the pair. The gate is "+
			"cached, so a daemon started before its env var was written never recovers.", n)
	}
}

// TestTheGateIsCheckedBeforeTheSessionHeaderWarning ordering, because the two
// interact: a lane that is not this machine's producer must not warn about the
// tool's headerless POSTs. It would be warning about traffic it is not
// responsible for.
func TestTheGateIsCheckedBeforeTheSessionHeaderWarning(t *testing.T) {
	em, _, warnings := electionEmitter(t, func() bool { return false })

	c := sampleCaptured()
	c.RequestHeaders = map[string]string{"Anthropic-Version": "2023-06-01"} // no session header

	em.Emit(context.Background(), c)
	if got := warnings.String(); got != "" {
		t.Errorf("an unelected lane warned about another lane's traffic: %q", got)
	}
}

// TestAFailedAppendAbandonsTheActivityRatherThanOrphaningAHalf is the regression
// for a defect this pairing work introduced and a review caught: the append loop
// used to `continue`, so a Started half that failed to spool while the Completed
// half succeeded produced exactly the single-sided activity the pairing exists to
// eliminate. Zero rows is a clean absence; one row is a contract violation that
// reads as a working record.
func TestAFailedAppendAbandonsTheActivityRatherThanOrphaningAHalf(t *testing.T) {
	// A delivery pool that refuses every submission (saturated): every Deliver
	// call returns false, the in-process equivalent of every spool Append
	// failing.
	delivery := newFakeDelivery()
	delivery.Accept = false

	var warnings bytes.Buffer
	em := &Emitter{
		Lane:    LaneProxy,
		Deliver: delivery.Deliver,
		DID:     func() string { return testDID },
		Warn:    func(format string, args ...any) { fmt.Fprintf(&warnings, format+"\n", args...) },
		Elected: func() bool { return true },
	}
	em.Emit(context.Background(), capturedWithSession("sess-1"))

	got := warnings.String()
	if got == "" {
		t.Fatal("a failed append said nothing")
	}
	if !strings.Contains(got, "abandoned") {
		t.Errorf("the warning does not say the activity was abandoned: %q", got)
	}
	// One warning, not one per half: the loop stops at the first failure.
	if n := strings.Count(got, "dropped event"); n != 1 {
		t.Errorf("warned %d times, want 1; the loop continued past a failure", n)
	}
}
