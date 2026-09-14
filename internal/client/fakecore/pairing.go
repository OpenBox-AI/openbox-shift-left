package fakecore

import (
	"fmt"
	"sort"
	"strings"
)

// Wire event types. The stored vocabulary is four values, not the 33-type
// DevEvent one: the body on the wire is a projection, so a grader reading it
// must speak the projection's language.
const (
	WireWorkflowStarted   = "WorkflowStarted"
	WireWorkflowCompleted = "WorkflowCompleted"
	WireSignalReceived    = "SignalReceived"
	WireActivityStarted   = "ActivityStarted"
	WireActivityCompleted = "ActivityCompleted"
)

// PairingGrader holds the binary to: every activity that RAN carries exactly
// two delivered rows, one started and one completed, checked per activity_id.
//
// Never by parity. A session's started+completed total is legitimately odd --
// a tool blocked at PreToolUse never ran, so no PostToolUse could fire and
// fabricating a completion for it would be worse than the asymmetry. Measured
// on a live 809-event session: parity over its 647 started+completed rows is
// odd and would reject a healthy session; per activity_id it names the three
// genuinely single-sided calls.
//
// The exemption needs a witness, and the wire cannot supply one: the
// enforcement ledger records the denial but carries no activity or tool id, so
// a grader reading only the inbox must either exempt every single-sided start
// (after which a dropped PostToolUse wrongly passes) or exempt none (parity
// again). The witness is therefore the scenario's own Denied set, joined to
// the wire through metadata.tool_use_id -- which the mapper writes
// structurally, so it survives content_capture:false.
//
// A declared denial is not a silencer. Denied says the call produced ONE row,
// so declaring a call denied that in fact completed is itself a reason:
// marking every call denied does not buy silence, it buys failures.
func PairingGrader() Grader {
	return Grader{
		Name:   "pairing",
		Check:  checkPairing,
		Mutate: func(s Scenario) Scenario { return Drop(s, "PostToolUse") },
	}
}

type activityGroup struct {
	started   int
	completed int
	toolUseID string
	toolName  string
}

func checkPairing(sc Scenario, inbox []Received) []string {
	groups := map[string]*activityGroup{}
	var order []string
	for _, r := range inbox {
		et := r.EventType()
		if et != WireActivityStarted && et != WireActivityCompleted {
			continue
		}
		id := r.ActivityID()
		g, ok := groups[id]
		if !ok {
			g = &activityGroup{}
			groups[id] = g
			order = append(order, id)
		}
		if et == WireActivityStarted {
			g.started++
		} else {
			g.completed++
		}
		// Either half may carry them; take the first non-empty.
		if g.toolUseID == "" {
			g.toolUseID = r.ToolUseID()
		}
		if g.toolName == "" {
			g.toolName = r.metaString("tool_name")
		}
	}
	sort.Strings(order)

	var reasons []string
	for _, id := range order {
		g := groups[id]
		// A tool activity with no tool_use_id would be graded through
		// pairKey's ToolCallStartKey fallback -- a path production does not
		// take, because every native tool payload carries the id. Grading it
		// would be grading the fallback.
		if g.toolName != "" && g.toolUseID == "" {
			reasons = append(reasons, fmt.Sprintf(
				"activity %s (tool %s): no metadata.tool_use_id on the wire, so the scenario cannot be joined to it",
				id, g.toolName))
			continue
		}

		denied := sc.Denied[g.toolUseID] && g.toolUseID != ""
		switch {
		case denied && g.completed > 0:
			reasons = append(reasons, fmt.Sprintf(
				"activity %s (tool %s, tool_use_id %s): the scenario says it was blocked before it ran, but %d ActivityCompleted row(s) were delivered",
				id, g.toolName, g.toolUseID, g.completed))
		case denied && g.started != 1:
			reasons = append(reasons, fmt.Sprintf(
				"activity %s (tool %s): blocked before it ran, so it must carry exactly 1 ActivityStarted; got %d",
				id, g.toolName, g.started))
		case denied:
			// One started row, no completion: the correct shape for a call
			// that never ran.
		case g.started == 1 && g.completed == 1:
			// Paired.
		case g.completed > 0 && g.started == 0:
			reasons = append(reasons, fmt.Sprintf(
				"activity %s (tool %s): %d ActivityCompleted row(s) with no ActivityStarted",
				id, g.toolName, g.completed))
		case g.started > 0 && g.completed == 0:
			reasons = append(reasons, fmt.Sprintf(
				"activity %s (tool %s, tool_use_id %s): ActivityStarted with no ActivityCompleted, and the scenario does not list it as blocked before it ran",
				id, g.toolName, g.toolUseID))
		default:
			// Distinct calls can share an activity_id when they share a tool,
			// file and operation id (activityPairKey), so this reason can mean
			// a collision rather than a duplicate delivery. Either way the
			// stated invariant -- exactly two rows -- does not hold for it.
			reasons = append(reasons, fmt.Sprintf(
				"activity %s (tool %s): %d ActivityStarted and %d ActivityCompleted rows; want exactly one of each",
				id, g.toolName, g.started, g.completed))
		}
	}
	return reasons
}

// Drop removes every payload for a hook event. It is a function over the
// payload list rather than a second fixture file, so the mutation is visible
// in the diff instead of hidden between two near-identical JSON blobs.
func Drop(sc Scenario, event string) Scenario {
	kept := make([]HookPayload, 0, len(sc.Payloads))
	for _, p := range sc.Payloads {
		if p.Event == event {
			continue
		}
		kept = append(kept, p)
	}
	sc.Payloads = kept
	sc.Name = sc.Name + "/dropped-" + strings.ToLower(event)
	return sc
}

// Undeny clears the witness. Used by the meta-test to prove the witness is
// load-bearing: without it the grader has to choose between exempting every
// single-sided start and rejecting healthy sessions.
func Undeny(sc Scenario) Scenario {
	sc.Denied = nil
	sc.Name = sc.Name + "/no-witness"
	return sc
}
