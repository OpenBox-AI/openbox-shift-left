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

// jsonMarshal is the package's one marshal seam, so a test building a synthetic
// row produces the same bytes a real one would.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
