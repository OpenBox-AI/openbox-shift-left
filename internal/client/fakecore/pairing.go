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

// activityTypeLLMCompletion labels a model turn's activity pair. Turns carry
// no tool_use_id because there is no tool call to identify, so they are the
// one legitimate class of activity row without one.
const activityTypeLLMCompletion = "llm_completion"

// activityTypeModelCallGate labels a model-call gate's activity pair: a pre-send
// decision, likewise with no tool call to identify.
const activityTypeModelCallGate = "model_call_gate"

// PairingGrader holds the binary to: every call that RAN is reported twice,
// once started and once completed; every call blocked before it ran is
// reported once.
//
// Never by parity. A session's started+completed total is legitimately odd --
// a tool denied at PreToolUse never ran, so no PostToolUse could fire and
// fabricating a completion would be worse than the asymmetry. Measured on a
// live 809-event session: parity over its 647 started+completed rows is odd
// and would reject a healthy session.
//
// Keyed per invocation (tool_use_id), NOT per activity_id. activity_id is
// derived from the call's arguments alone, so a retry that repeats the same
// arguments deliberately resolves to the same activity_id -- that is what lets
// an approved request be consumed by its retry, and it is pinned by
// TestApprovalRetry_NewInvocationIDSameArgsSharesActivityID. Counting rows per
// activity_id would call that healthy session a double delivery. What the two
// halves of ONE invocation must do is agree on an activity_id, and that is
// asserted instead.
//
// The single-sided exemption needs a witness the wire cannot supply: a denied
// call's row was sent BEFORE the verdict existed -- it is the evaluation
// request -- so it cannot mark itself denied, and the enforcement ledger
// carries no activity or tool id. The scenario declares it. But a declaration
// is not trusted: it must agree with the decision the binary actually rendered
// on stdout, which is a different surface produced by a different code path.
// So declaring a call denied that was not denied is a failure, and declaring
// every call denied buys failures rather than silence.
func PairingGrader() Grader {
	return Grader{
		Name:   "pairing",
		Check:  checkPairing,
		Mutate: func(s Scenario) (Scenario, []string) { return Drop(s, "PostToolUse"), DroppedIDs(s, "PostToolUse") },
	}
}

type activityGroup struct {
	// gate marks a model-call gate, whose completion is optional: a pre-send
	// deny leaves the started row only, and no completion is fabricated.
	gate        bool
	started     int
	completed   int
	toolName    string
	activityIDs map[string]bool
}

func newGroup() *activityGroup { return &activityGroup{activityIDs: map[string]bool{}} }

func (g *activityGroup) add(r Received) {
	if r.EventType() == WireActivityStarted {
		g.started++
	} else {
		g.completed++
	}
	if g.toolName == "" {
		g.toolName = r.metaString("tool_name")
	}
	if id := r.ActivityID(); id != "" {
		g.activityIDs[id] = true
	}
}

func (g *activityGroup) ids() string {
	out := make([]string, 0, len(g.activityIDs))
	for id := range g.activityIDs {
		out = append(out, id)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func checkPairing(sc Scenario, run Run) []string {
	byCall := map[string]*activityGroup{}     // tool_use_id -> rows
	byActivity := map[string]*activityGroup{} // activity_id -> rows, for turns
	var callOrder, activityOrder []string
	var reasons []string

	for _, r := range run.Inbox {
		if et := r.EventType(); et != WireActivityStarted && et != WireActivityCompleted {
			continue
		}
		if id := r.ToolUseID(); id != "" {
			g, ok := byCall[id]
			if !ok {
				g = newGroup()
				byCall[id] = g
				callOrder = append(callOrder, id)
			}
			g.add(r)
			continue
		}
		// No tool_use_id. A model turn or a model-call gate legitimately has none; anything else
		// has lost its join key, and grading it would grade the fallback path
		// production does not take.
		at, _ := r.Body["activity_type"].(string)
		if at != activityTypeLLMCompletion && at != activityTypeModelCallGate {
			reasons = append(reasons, fmt.Sprintf(
				"activity %s (activity_type %q): no metadata.tool_use_id on the wire, so no scenario input can be joined to it",
				r.ActivityID(), at))
			continue
		}
		id := r.ActivityID()
		g, ok := byActivity[id]
		if !ok {
			g = newGroup()
			g.gate = at == activityTypeModelCallGate
			byActivity[id] = g
			activityOrder = append(activityOrder, id)
		}
		g.add(r)
	}

	sort.Strings(callOrder)
	sort.Strings(activityOrder)

	for _, tu := range callOrder {
		reasons = append(reasons, checkOneCall(sc, run, tu, byCall[tu])...)
	}
	for _, id := range activityOrder {
		g := byActivity[id]
		kind, rule := "model turn", "a turn is one pair"
		torn := g.started != 1 || g.completed != 1
		if g.gate {
			kind, rule = "model-call gate", "a gate is a started row, and at most one completed row"
			torn = g.started != 1 || g.completed > 1
		}
		if torn {
			reasons = append(reasons, fmt.Sprintf(
				"%s %s: %d started and %d completed rows; %s",
				kind, id, g.started, g.completed, rule))
		}
	}
	return reasons
}

func checkOneCall(sc Scenario, run Run, toolUseID string, g *activityGroup) []string {
	var reasons []string
	label := fmt.Sprintf("call %s (tool %s)", toolUseID, g.toolName)

	// The two halves of one invocation must land on one activity_id, or the
	// pair is torn and the timeline shows two half-rows instead of one call.
	if len(g.activityIDs) > 1 {
		reasons = append(reasons, fmt.Sprintf(
			"%s: its rows carry %d different activity_ids (%s); one call's halves must pair onto one row",
			label, len(g.activityIDs), g.ids()))
	}

	declaredDenied := sc.Denied[toolUseID]
	observed, sawDecision := run.DecisionFor(toolUseID)

	// The declaration and the rendered decision are two independently produced
	// surfaces. They must agree, or one of them is wrong and the exemption
	// below would be resting on a fiction.
	switch {
	case declaredDenied && (!sawDecision || observed.Verb != "deny"):
		reasons = append(reasons, fmt.Sprintf(
			"%s: the scenario says it was blocked before it ran, but the binary rendered %q to the coding agent",
			label, observed.Verb))
	case !declaredDenied && sawDecision && observed.Verb == "deny":
		reasons = append(reasons, fmt.Sprintf(
			"%s: the binary denied it, but the scenario does not list it as blocked before it ran",
			label))
	}

	if declaredDenied {
		if g.completed > 0 {
			reasons = append(reasons, fmt.Sprintf(
				"%s: blocked before it ran, yet %d ActivityCompleted row(s) were delivered",
				label, g.completed))
		}
		if g.started != 1 {
			reasons = append(reasons, fmt.Sprintf(
				"%s: blocked before it ran, so it must be reported exactly once; got %d ActivityStarted row(s)",
				label, g.started))
		}
		return reasons
	}

	if g.started != 1 || g.completed != 1 {
		reasons = append(reasons, fmt.Sprintf(
			"%s: %d ActivityStarted and %d ActivityCompleted rows; a call that ran is reported exactly once each, and the scenario does not list it as blocked before it ran",
			label, g.started, g.completed))
	}
	return reasons
}

// Drop removes every payload for a hook event and reports which calls lost a
// half. It is a function over the payload list rather than a second fixture
// file, so the mutation is visible in the diff instead of hidden between two
// near-identical JSON blobs -- and the returned ids let the meta-test require
// that a grader NAMES what the mutation took, which it can only do by
// noticing.
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

// DroppedIDs reports the tool_use_ids that lose a payload when event is
// dropped from sc.
func DroppedIDs(sc Scenario, event string) []string {
	var ids []string
	for _, p := range sc.Payloads {
		if p.Event == event {
			if id := p.ToolUseID(); id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}
