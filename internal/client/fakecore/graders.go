package fakecore

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The grader set. Two families: transcript -- what the binary reported -- and
// outcome -- what it actually did. Both are required. Outcome alone misses a
// correct stream describing a block that never happened; transcript alone
// misses a correct block that was never reported to the control plane.
//
// Every expectation below is derived from the scenario's INPUT. That is the
// whole discipline: an expectation read out of the inbox it is checking is an
// identity. "W + 2A - singles == len(inbox)" is arithmetic that cannot fail.
//
// Two properties are deliberately NOT graded here, because re-asserting them
// would inflate what this suite appears to cover:
//
//   - No body carries a spans key. The fake refuses such a body at the door,
//     and the property already has three owners: gatewayemit/event_test.go,
//     gatewayemit/lane_test.go and conformance_parity_test.go.
//   - Lane disjointness is owned by gatewayemit's TestTheLanesAreDisjoint.
//
// Thinking is also out of scope: it needs a transcript_path fixture the native
// payload format has no slot for. content_conformance_test.go owns it.

// expectedRows is what the scenario's payload list says must reach the wire.
// One entry per row the input demands, before a single delivered body is read.
type expectedRow struct {
	eventType string
	toolUseID string // "" for session and signal rows
	signal    string
	from      string // the payload that demands it, for the reason text
}

// expect projects the payload list onto the rows it must produce.
//
// A completion is expected for every gated call the scenario does NOT declare
// blocked -- taken from the PreToolUse payload, never from the PostToolUse
// one. Deriving it from the PostToolUse payload would make the whole grader
// unfalsifiable: dropping that payload would drop the expectation with it, and
// the loss it exists to catch would pass.
func expect(sc Scenario) []expectedRow {
	var want []expectedRow
	for _, p := range sc.Payloads {
		switch p.Event {
		case "SessionStart":
			want = append(want, expectedRow{eventType: WireWorkflowStarted, from: p.Event})
		case "SessionEnd":
			want = append(want, expectedRow{eventType: WireWorkflowCompleted, from: p.Event})
		case "UserPromptSubmit":
			want = append(want, expectedRow{eventType: WireSignalReceived, signal: "prompt_submitted", from: p.Event})
		case "PreToolUse":
			id := p.ToolUseID()
			want = append(want, expectedRow{eventType: WireActivityStarted, toolUseID: id, from: p.Event})
			if !sc.Denied[id] {
				want = append(want, expectedRow{eventType: WireActivityCompleted, toolUseID: id, from: p.Event + " (the call was not blocked, so it ran)"})
			}
		}
	}
	return want
}

func rowKey(eventType, toolUseID, signal string) string {
	return eventType + "\x1f" + toolUseID + "\x1f" + signal
}

func deliveredKeys(inbox []Received) map[string]int {
	got := map[string]int{}
	for _, r := range inbox {
		signal, _ := r.Body["signal_name"].(string)
		got[rowKey(r.EventType(), r.ToolUseID(), signal)]++
	}
	return got
}

// CompletenessGrader: nothing is lost between stdin and the wire.
//
// This is what the retired shell suite's "total = W + 2A + S" was reaching
// for, but that sum was computed from the rows it was checking, so it could
// only ever restate them.
func CompletenessGrader() Grader {
	return Grader{
		Name:   "completeness",
		Mutate: func(s Scenario) (Scenario, []string) { return Drop(s, "PostToolUse"), DroppedIDs(s, "PostToolUse") },
		Check: func(sc Scenario, run Run) []string {
			got := deliveredKeys(run.Inbox)
			var reasons []string
			for _, w := range expect(sc) {
				k := rowKey(w.eventType, w.toolUseID, w.signal)
				if got[k] == 0 {
					reasons = append(reasons, fmt.Sprintf(
						"the %s payload demands a %s row%s and none was delivered",
						w.from, w.eventType, describeSubject(w)))
					continue
				}
				got[k]--
			}
			// Anything left over was delivered without an input asking for it.
			var extra []string
			for k, n := range got {
				if n > 0 {
					parts := strings.Split(k, "\x1f")
					extra = append(extra, fmt.Sprintf("%d x %s%s", n, parts[0], describeSubject(expectedRow{toolUseID: parts[1], signal: parts[2]})))
				}
			}
			sort.Strings(extra)
			for _, e := range extra {
				reasons = append(reasons, "delivered with nothing in the input asking for it: "+e)
			}
			return reasons
		},
	}
}

func describeSubject(w expectedRow) string {
	switch {
	case w.toolUseID != "":
		return " for call " + w.toolUseID
	case w.signal != "":
		return " named " + w.signal
	}
	return ""
}

// ActivityTypeGrader: the label the dashboard shows is the tool the developer
// actually invoked, on BOTH halves of the call.
//
// Taken from the PreToolUse payload and checked against every row for that
// call. Read off a delivered row alone it would be intra-row -- the mapper
// writes the label and the id from one source, so they cannot disagree. What
// can disagree is the two halves with each other, which is a call appearing on
// the timeline under two names.
func ActivityTypeGrader() Grader {
	return Grader{
		Name: "activity-type",
		Mutate: func(s Scenario) (Scenario, []string) {
			return RelabelCompletion(s, "Read"), DroppedIDs(s, "PostToolUse")
		},
		Check: func(sc Scenario, run Run) []string {
			want := map[string]string{}
			for _, p := range sc.Payloads {
				if p.Event == "PreToolUse" {
					want[p.ToolUseID()] = p.toolName()
				}
			}
			var reasons []string
			for _, r := range run.Inbox {
				id := r.ToolUseID()
				if id == "" {
					continue
				}
				expected, ok := want[id]
				if !ok || expected == "" {
					continue
				}
				if at, _ := r.Body["activity_type"].(string); at != expected {
					reasons = append(reasons, fmt.Sprintf(
						"call %s was invoked as %s, but its %s row is labelled %q",
						id, expected, r.EventType(), at))
				}
			}
			return reasons
		},
	}
}

// DeliveryOnceGrader: one call, one row of each kind.
//
// Keyed per invocation, not per activity_id: a retry repeating the same
// arguments shares an activity_id on purpose, and counting by that would call
// a healthy retry a duplicate.
//
// It asserts that this client emits one row per kind. It cannot assert that
// the control plane keeps them apart -- that is the far end of the wire, and
// nothing in this repository observes it.
func DeliveryOnceGrader() Grader {
	return Grader{
		Name:   "delivery-once",
		Mutate: func(s Scenario) (Scenario, []string) { return Duplicate(s, "PreToolUse"), DroppedIDs(s, "PreToolUse") },
		Check: func(_ Scenario, run Run) []string {
			seen := map[string]int{}
			for _, r := range run.Inbox {
				if id := r.ToolUseID(); id != "" {
					seen[id+"\x1f"+r.EventType()]++
				}
			}
			keys := make([]string, 0, len(seen))
			for k := range seen {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var reasons []string
			for _, k := range keys {
				if seen[k] > 1 {
					parts := strings.Split(k, "\x1f")
					reasons = append(reasons, fmt.Sprintf(
						"call %s was delivered %d %s rows; the control plane does not dedupe developer events, so each extra one is another row on the timeline",
						parts[0], seen[k], parts[1]))
				}
			}
			return reasons
		},
	}
}

// SignalArgsGrader: a prompt's signal_args carries the prompt the developer
// typed, and carries it only when content capture is on.
//
// prompt_submitted is the one signal whose signal_args is read as the session
// goal rather than as its own payload, which is why it is graded on its own.
// The expected prompt is a fixed argument, not read from the payload being
// checked. Reading it from there would make the grader unfalsifiable: removing
// the prompt would remove the expectation with it, and the loss would pass.
func SignalArgsGrader(prompt string) Grader {
	return Grader{
		Name:   "signal-args",
		Mutate: func(s Scenario) (Scenario, []string) { return StripCanary(s, prompt), nil },
		Check: func(sc Scenario, run Run) []string {
			var reasons []string
			row, ok := findSignal(run.Inbox, "prompt_submitted")
			if !ok {
				return []string{"no prompt_submitted signal reached the wire, so the session has no goal"}
			}
			args, _ := json.Marshal(row.Body["signal_args"])
			carries := strings.Contains(string(args), prompt)
			if sc.Posture.ContentCapture == "1" && !carries {
				reasons = append(reasons, "content capture is on but the prompt_submitted signal_args does not carry the prompt, so the session has no goal")
			}
			if sc.Posture.ContentCapture != "1" && carries {
				reasons = append(reasons, "content capture is off but the prompt_submitted signal_args carries the prompt text")
			}
			// The one signal whose args are read as the goal rather than as
			// its own payload; every other signal's args are its payload, and
			// core's source-and-name gate is the only thing keeping the two
			// apart.
			for _, r := range run.Inbox {
				if r.EventType() != WireSignalReceived {
					continue
				}
				if name, _ := r.Body["signal_name"].(string); name != "prompt_submitted" {
					if _, present := r.Body["signal_args"]; !present {
						reasons = append(reasons, "signal "+name+" carries no signal_args, so its payload was lost")
					}
				}
			}
			return reasons
		},
	}
}

// StripCanary rewrites a known string out of every payload, leaving the
// posture alone. It is the mutation for a grader whose expectation is a fixed
// value: the value stops arriving while the grader goes on expecting it.
func StripCanary(sc Scenario, canary string) Scenario {
	out := make([]HookPayload, 0, len(sc.Payloads))
	for _, p := range sc.Payloads {
		p.JSON = strings.ReplaceAll(p.JSON, canary, "ordinary-text")
		out = append(out, p)
	}
	sc.Payloads = out
	sc.Name += "/canary-removed"
	return sc
}

func findSignal(inbox []Received, name string) (Received, bool) {
	for _, r := range inbox {
		if r.EventType() == WireSignalReceived {
			if got, _ := r.Body["signal_name"].(string); got == name {
				return r, true
			}
		}
	}
	return Received{}, false
}

// toolName and prompt read the raw native payload with anonymous structs, not
// the adapter's own HookEvent: binding that type here would make a mis-tagged
// field read "" on both sides at once.
func (p HookPayload) toolName() string {
	var got struct {
		ToolName string `json:"tool_name"`
	}
	_ = json.Unmarshal([]byte(p.JSON), &got)
	return got.ToolName
}

// RelabelCompletion rewrites the tool name on every PostToolUse payload, so a
// call's two halves disagree about what tool it was.
func RelabelCompletion(sc Scenario, tool string) Scenario {
	out := make([]HookPayload, 0, len(sc.Payloads))
	for _, p := range sc.Payloads {
		if p.Event == "PostToolUse" {
			p.JSON = replaceJSONString(p.JSON, "tool_name", tool)
		}
		out = append(out, p)
	}
	sc.Payloads = out
	sc.Name += "/completion-relabelled"
	return sc
}

// Duplicate repeats every payload for an event, which is the shape a double
// delivery takes on the wire.
func Duplicate(sc Scenario, event string) Scenario {
	out := make([]HookPayload, 0, len(sc.Payloads)+1)
	for _, p := range sc.Payloads {
		out = append(out, p)
		if p.Event == event {
			out = append(out, p)
		}
	}
	sc.Payloads = out
	sc.Name += "/duplicated-" + strings.ToLower(event)
	return sc
}

// runKeyFor is the (workflow_id, run_id) pair StartFirst groups rows by: the
// same scope core's own HALT/latch pre-check is evaluated against, so a row
// belongs to the run named on its OWN wire body, never to the scenario's
// session id (a run bump keeps the session id but mints a new run_id).
func runKeyFor(r Received) string {
	return r.topString("workflow_id") + "\x1f" + r.topString("run_id")
}

// ReorderGatedCallBeforeSessionStart moves sc's own first gated payload
// (PreToolUse or UserPromptSubmit) to the FRONT of the payload list, ahead
// of SessionStart: the shape StartFirst exists to catch (a gated call's own
// live escalation reaching core before the session's own inline
// WorkflowStarted attempt has even been made), produced here by input order
// alone -- the binary drives whatever native hook fires first, in the order
// it is told to. A no-op (sc returned unchanged) when the scenario has no
// gated payload to move.
func ReorderGatedCallBeforeSessionStart(sc Scenario) Scenario {
	var gated *HookPayload
	out := make([]HookPayload, 0, len(sc.Payloads))
	for i := range sc.Payloads {
		if gated == nil && (sc.Payloads[i].Event == "PreToolUse" || sc.Payloads[i].Event == "UserPromptSubmit") {
			p := sc.Payloads[i]
			gated = &p
			continue
		}
		out = append(out, sc.Payloads[i])
	}
	if gated == nil {
		return sc
	}
	sc.Payloads = append([]HookPayload{*gated}, out...)
	sc.Name += "/gated-call-reordered-before-session-start"
	return sc
}

// StartFirst: no ActivityStarted/ActivityCompleted/SignalReceived row of a
// run reaches core before that SAME run's own WorkflowStarted did. Read off
// Inbox arrival order -- delivery order equals append order (the spool's own
// contract) -- so this is a property of what actually reached the wire, not
// of the input list's order.
//
// WorkflowCompleted is deliberately exempt: SessionEnd's own inline attempt
// is unconditional, running even for a run a halt latch already covers (a
// halted run's remaining events still get their own one attempt; failures
// there do not change the latch), so it can legitimately reach core (or
// fail) independently of whether its run's own WorkflowStarted was ever
// accepted -- checking it here would misgrade the exact recovery shape a
// core-down-at-session-start scenario exists to prove (the session's start
// failed, but its own end is still attempted once, on its own schedule).
//
// A run whose WorkflowStarted was never accepted at all (an outage at
// session start) is not a violation by omission for the rows this DOES
// check: every later GATED call of that run denies from the halt latch
// without ever reaching the wire, so the loop below finds nothing to
// complain about.
//
// Mutate moves the scenario's own first gated payload (PreToolUse or
// UserPromptSubmit) ahead of SessionStart, rather than dropping SessionStart
// outright: dropping it would ALSO fail every other grader's own "the
// mutated run is still a working session" pre-check (exactly one
// WorkflowStarted, exactly one WorkflowCompleted), which is not the failure
// this grader exists to prove. Reordering keeps SessionStart in the input
// (still exactly one WorkflowStarted/WorkflowCompleted pair) while making
// the gated call's own escalation the first thing this run ever sends. It
// touches no PARTICULAR call (every later row is affected alike), so it
// returns no ids, which the Grader contract allows.
func StartFirst() Grader {
	return Grader{
		Name:   "start-first",
		Mutate: func(s Scenario) (Scenario, []string) { return ReorderGatedCallBeforeSessionStart(s), nil },
		Check: func(_ Scenario, run Run) []string {
			started := map[string]bool{}
			var reasons []string
			for _, r := range run.Inbox {
				key := runKeyFor(r)
				switch et := r.EventType(); et {
				case WireWorkflowStarted:
					started[key] = true
				case WireWorkflowCompleted:
					// Exempt; see the doc comment above.
				default:
					if !started[key] {
						reasons = append(reasons, fmt.Sprintf(
							"a %s row reached core before run %q's own WorkflowStarted was ever accepted",
							et, key))
					}
				}
			}
			return reasons
		},
	}
}

// OneAttempt: every Idempotency-Key this run's own traffic used was attempted
// exactly once, plus one more attempt for each time this run's own script
// held that same key's response past the caller's budget (the one
// legitimate re-send this plan accepts: an attempt still unanswered when a
// gate's own drain had to stop waiting goes back to the queue for the 30s
// drainers, deduped by core on this same key). Any other repeat is a real
// double delivery, never loosened away.
//
// No Mutate: a same-key double-send cannot be produced by any sequence of
// DISTINCT native hook payloads. A first attempt at Mutate here repeated a
// PreToolUse payload verbatim (Duplicate), on the assumption that two
// separate hook invocations mapping identical arguments derive the
// identical event id -- wrong, caught by this Mutate itself staying green
// against its own mutation: claude-code's own mapper derives EventID from
// the arguments AND a high-resolution timestamp taken fresh inside each
// hook process (mapper.go's own deriveID, INV-5's own per-event
// distinguisher, deliberately so two distinct calls never collide), so two
// separate invocations of identical content still mint two DIFFERENT keys,
// each correctly attempted once -- not a violation. The only way the same
// key is EVER attempted twice is the one legitimate case HeldByKey already
// reconciles (the SAME already-mapped, already-timestamped event requeued
// after a held response), which no input mutation can turn into an
// UNRECONCILED repeat; the only thing that could make this grader fail is a
// code regression in that reconciliation itself. Proven red directly,
// against a hand-built Run, by internal/client/fakecore/graderreasons_test.go's
// own table -- the same treatment RedactionGrader's and HaltedAfterFailure's
// own un-switchable properties get.
func OneAttempt() Grader {
	return Grader{
		Name: "one-attempt",
		Check: func(_ Scenario, run Run) []string {
			keys := make([]string, 0, len(run.AttemptsByKey))
			for k := range run.AttemptsByKey {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var reasons []string
			for _, k := range keys {
				held := run.HeldByKey[k]
				want := 1 + held
				if got := run.AttemptsByKey[k]; got != want {
					reasons = append(reasons, fmt.Sprintf(
						"idempotency key %s was attempted %d time(s), want %d (one attempt, plus %d requeued after a held response); an unexplained repeat is a double delivery",
						k, got, want, held))
				}
			}
			return reasons
		},
	}
}

// gatedHookEvents are the native hook events that ever reach a gate, across
// both providers: Codex's own set adds PermissionRequest and drops
// ConfigChange (a Claude Code-only hook), so this is their union. Read
// against Decision.Event, never assumed from a caller's own filtering (a
// synthetic Run, or a future scenario, might not pre-filter for us).
var gatedHookEvents = map[string]bool{
	"PreToolUse":        true,
	"UserPromptSubmit":  true,
	"ConfigChange":      true,
	"PermissionRequest": true,
}

// isHaltReplyReason reports whether a rendered deny's own reason text is one
// of the two FIXED phrases this codebase ever writes for a run that cannot
// continue on its own: a delivery failure (HaltOnDeliveryFailure) or a HALT
// verdict replayed from an unreadable/corrupt latch (SessionHaltDecision's
// own generic fallback, used only when the latch carries no reason at all).
// This alone is NOT sufficient to detect every halt: a live HALT verdict
// carries the POLICY's own reason text verbatim (e.g. "org kill switch"),
// never this fixed wording, so HaltedAfterFailure's own Check also reads
// run.Ledger and the rendered Stop lever -- see its own doc comment.
func isHaltReplyReason(reason string) bool {
	return strings.Contains(reason, "OpenBox could not record") || strings.Contains(reason, "session halted")
}

// ledgerAppliedHalt reports whether one decoded enforcement-ledger row is
// the row for a decision that actually set the session-halt lever
// (EnforcementRecord.AppliedDecision, "halt" iff dec.SessionHalt was true --
// hookflow/enforce.go). This is the one signal that survives identically
// across BOTH providers' own contracts: Claude Code's tool contracts DO
// render `continue:false` for a HALT (TestSessionHaltConformance "C27 tool
// HALT denies, stops the session, and latches"), but Codex's tool contracts
// (PreToolUse, PermissionRequest) fold a HALT into a plain per-call deny
// with no stop lever at all -- only Codex's OWN prompt contract latches --
// so a rendered Decision alone cannot tell a Codex tool HALT apart from an
// ordinary BLOCK. The ledger can, because it records the decision BEFORE
// either provider's contract renders anything.
func ledgerAppliedHalt(rec map[string]any) bool {
	ad, _ := rec["applied_decision"].(string)
	return ad == "halt"
}

// HaltedAfterFailure: once a run's own gated decision halts -- a delivery
// failure, or ANY HALT verdict, whether replayed from the latch with the
// fixed generic reason this codebase writes for an unreadable latch, or a
// live evaluation naming its own policy-authored reason -- EVERY later
// GATED decision of that same run must also deny, including one that
// rendered NOTHING at all (Verb == "", a silent ALLOW slipping through is
// exactly the failure mode a halt exists to prevent). An ungated event
// carries no verdict to check and is skipped in both directions.
//
// Halted is detected from three independent signals, ORed together, because
// no single one covers every shape: run.Ledger's own applied_decision field
// (index-aligned with run.Decisions -- true for every event class this
// suite's own runner puts into Decisions today, since every one of them
// unconditionally writes exactly one ledger row via RecordEnforcement) is
// the most robust and catches a policy-authored HALT reason on ANY gated
// class; the rendered Stop lever catches a live HALT on a contract that
// renders one (the prompt contract, both providers); the fixed-phrase check
// catches a REPLAYED halt whose own latch carries no reason at all.
//
// Scoped PER SESSION (Decision.Payload indexes back into sc.Payloads for its
// own session_id), never across the whole Run: a scenario proving recovery
// (a halted session followed by an unrelated, freshly-started one) must not
// have the SECOND session's own gated call read as "still halted" merely
// because it comes later in the same Decisions slice.
//
// No Mutate: this is not a property an input mutation can switch off. A
// scenario cannot make a healthy binary "un-halt" itself for one call and
// not the next; the only thing that could make this grader fail is a code
// regression in the latch read on a later gated call, which no scenario
// fixture expresses. Proven red directly, against hand-built Runs, in
// internal/client/fakecore/graderreasons_test.go's own table -- the same
// treatment RedactionGrader's own un-switchable property gets.
func HaltedAfterFailure() Grader {
	return Grader{
		Name: "halted-after-failure",
		Check: func(sc Scenario, run Run) []string {
			var reasons []string
			haltedOn := map[string]string{} // session id -> its own halting reason
			for i, d := range run.Decisions {
				sessionID := ""
				if d.Payload >= 0 && d.Payload < len(sc.Payloads) {
					sessionID = sc.Payloads[d.Payload].SessionID()
				}
				ledgerHalted := i < len(run.Ledger) && ledgerAppliedHalt(run.Ledger[i])
				reason, halted := haltedOn[sessionID]
				if !halted {
					if ledgerHalted || d.Stop || isHaltReplyReason(d.Reason) {
						reason = d.Reason
						if reason == "" {
							reason = "session halted"
						}
						haltedOn[sessionID] = reason
					}
					continue
				}
				if !gatedHookEvents[d.Event] {
					continue // ungated: carries no verdict, out of scope
				}
				if d.Verb != "deny" && d.Verb != "block" {
					verb := d.Verb
					if verb == "" {
						verb = "a silent allow"
					}
					reasons = append(reasons, fmt.Sprintf(
						"payload #%d (%s, call %s): the run was already halted (%q), but this call rendered %q instead of a halt deny",
						d.Payload, d.Event, d.ToolUseID, reason, verb))
				}
			}
			return reasons
		},
	}
}

func replaceJSONString(raw, key, value string) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return raw
	}
	v, _ := json.Marshal(value)
	m[key] = v
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return string(out)
}
