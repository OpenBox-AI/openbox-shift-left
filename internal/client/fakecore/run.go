package fakecore

import "encoding/json"

// Decision is what the coding agent actually read on stdout for one payload,
// decoded into a provider-neutral shape. It is the second surface a grader can
// join on, and the one that matters: the agent obeys these bytes and nothing
// else.
//
// Produced by the runner rather than by a grader, because rendering is
// adapter-owned and graders stay provider-neutral.
type Decision struct {
	Payload   int    // index into Scenario.Payloads
	Event     string // the native hook event name
	ToolUseID string // taken from the INPUT payload, not from any output
	Verb      string // deny | ask | allow | "" when the hook stayed silent
	Reason    string
	Stop      bool // continue:false -- the session-stop lever
}

// Run is everything one scenario produced. Graders read it whole: a correct
// event stream describing a block that did not happen is still a failure, and
// a correct block never reported to the control plane is also a failure.
type Run struct {
	// Inbox is every request the fake accepted, in arrival order, never
	// collapsed.
	Inbox []Received
	// Decisions is index-aligned with nothing: it holds one entry per payload
	// that produced output, carrying its own Payload index.
	Decisions []Decision
	// Ledger is the enforcement audit sink, one decoded object per line.
	Ledger []map[string]any
	// Dir is the scenario's private state directory: halt latch,
	// pending-approval markers, spool.
	Dir string
}

// DecisionFor returns the decision recorded for a tool call, and whether one
// was observed at all. Silence is not absence: a gated payload that produced
// no decision has an entry with an empty Verb, and a payload that was never
// gated has no entry. A grader must be able to tell those apart.
func (r Run) DecisionFor(toolUseID string) (Decision, bool) {
	for _, d := range r.Decisions {
		if d.ToolUseID != "" && d.ToolUseID == toolUseID {
			return d, true
		}
	}
	return Decision{}, false
}

// ToolUseID reads the identifier out of the raw native payload.
//
// Decoded with an anonymous struct on purpose. Binding the adapter's own
// HookEvent here would make the oracle and the mapper agree by construction --
// a mis-tagged field would make both read "", and the grader would confirm the
// bug rather than catch it.
func (p HookPayload) ToolUseID() string {
	var got struct {
		ToolUseID string `json:"tool_use_id"`
	}
	_ = json.Unmarshal([]byte(p.JSON), &got)
	return got.ToolUseID
}

// SessionID reads the session out of the raw native payload. A payload with no
// session is dropped fail-open by the mapper, which would silently falsify any
// expectation derived from the payload list -- so fixtures are checked for it.
func (p HookPayload) SessionID() string {
	var got struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal([]byte(p.JSON), &got)
	return got.SessionID
}
