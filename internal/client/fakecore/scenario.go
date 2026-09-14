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
	// Status forces an HTTP status on the nth accepted request (1-based).
	Status map[int]int
	// Posture is the run's configuration. Empty fields take the suite
	// default, which a bool could not express.
	Posture Posture
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
	ApprovalHoldMS string
}

// Script projects the scenario onto what the fake should answer.
func (s Scenario) Script() Script {
	return Script{Verdicts: s.Verdicts, Status: s.Status}
}

// Grader is a predicate over (scenario, inbox) returning reasons, not a
// boolean: a grader that says only "false" cannot be acted on.
//
// Check reads the SCENARIO for its expectation and the Run for what happened.
// An expectation read out of the same inbox it is checking is an identity, not
// a property.
//
// Mutate returns the scenario that must make this grader fail. Every grader
// ships one and the suite executes it, because a grader nobody has watched
// fail is not yet a grader.
type Grader struct {
	Name   string
	Check  func(Scenario, Run) []string
	Mutate func(Scenario) Scenario
}
