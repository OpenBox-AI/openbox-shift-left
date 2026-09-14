package fakecore

// HookPayload is one native hook invocation: the provider's event name and the
// JSON the tool writes to the binary's stdin. A session is an ordered list of
// these, which is the whole reason the model can be taken out of the loop --
// its only job was generating this list.
type HookPayload struct {
	Event string
	JSON  string
}

// Scenario is one governance claim, expressed as the input that produces it.
//
// Name is the claim, not the code path: an outside reader sees these in `go
// test -v` output and should be able to tell what the binary is being held to.
type Scenario struct {
	Name     string
	Payloads []HookPayload
	// Verdicts is what core answers, keyed by tool_use_id.
	Verdicts map[string]string
	// Denied names the tool calls that were blocked BEFORE they ran, so no
	// completion could fire. It is the pairing grader's witness, and it lives
	// here rather than being read off the wire because EnforcementRecord
	// carries no activity or tool id -- see PairingGrader.
	Denied map[string]bool
	// AlwaysStatus, when non-zero, answers every request with this status.
	AlwaysStatus int
	// Approval answers the approval poll. A scenario scripting
	// REQUIRE_APPROVAL without one runs the degraded 404 path, which denies
	// for the wrong reason.
	Approval func(Received) (int, string)
	// Posture is the run's configuration. Empty fields take the suite
	// default, which a bool could not express.
	Posture Posture
	// OutageDuring reports, per payload, whether the control plane is down
	// while that hook runs. Expressed over the payload list rather than a
	// request count so it never couples to the retry schedule.
	OutageDuring func(payloadIndex int, event string) bool
	// Provenance says where the payloads came from: recorded from a real
	// session (with the date), or authored. A field on the scenario, never a
	// key inside a native payload, which would change the shape under test.
	Provenance string
}

// Posture holds the environment knobs a scenario varies. Strings, not bools:
// these are env values and "" genuinely means "say nothing and take the
// default", which is a third state a bool cannot carry.
type Posture struct {
	Enforce        string // "0" | "1"
	FailClosed     string // "0" | "1"
	ContentCapture string // "0" | "1"
	// SecretDetection is "" (the default, on) or "0". Turning it off is how a
	// test proves the redactor is what keeps a secret off the wire.
	SecretDetection string
	ApprovalHoldMS  string
}

// Script projects the scenario onto what the fake should answer.
func (s Scenario) Script() Script {
	return Script{Verdicts: s.Verdicts, AlwaysStatus: s.AlwaysStatus}
}

// Grader is a predicate over (scenario, inbox) returning reasons, not a
// boolean: a grader that says only "false" cannot be acted on.
//
// Check reads the SCENARIO for its expectation and the Run for what happened.
// An expectation read out of the same inbox it is checking is an identity, not
// a property.
//
// Mutate returns the scenario that must make this grader fail, together with
// the calls it interfered with. Every grader ships one and the suite executes
// it, because a grader nobody has watched fail is not yet a grader; and the
// returned ids let the suite require that the grader NAMES what was taken,
// which it can only do by having noticed.
//
// A mutation that touches no particular call returns no ids, and is held to
// going red without the naming requirement.
type Grader struct {
	Name   string
	Check  func(Scenario, Run) []string
	Mutate func(Scenario) (Scenario, []string)
}
