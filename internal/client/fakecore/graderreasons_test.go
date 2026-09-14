package fakecore

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Every reason a grader can emit is reachable, and most are exercised here
// against a hand-built run. The rest are reached by the scenario tests in
// cmd/openbox, which drive the real binary -- this file does not duplicate
// those. What the pair guarantees together, and what TestEveryReasonSiteHasACase
// enforces, is that no reason is reachable by nothing.
//
// The mutation meta-test proves each grader can go red. It proves it by ONE
// route -- the grader's declared mutation -- and says nothing about the other
// reasons the same grader can return. Combined with a deliberately small
// fixture set (one tool class, one signal class, no model turns, no torn
// pairs), that left sixteen reason branches that read as assertions and could
// never fire. A reason nobody can trigger looks exactly like one that works.
//
// Synthetic rather than scenario-driven on purpose: several of these shapes are
// ones the binary should never produce, so no fixture can make them. What is
// being checked is the grader, not the binary.

// row builds one delivered row. Only the keys a grader reads are set.
func row(eventType, activityID, toolUseID string, extra map[string]any) Received {
	body := map[string]any{"event_type": eventType}
	if activityID != "" {
		body["activity_id"] = activityID
	}
	if toolUseID != "" {
		body["metadata"] = map[string]any{"tool_use_id": toolUseID, "tool_name": "Bash"}
	}
	for k, v := range extra {
		body[k] = v
	}
	return Received{Body: body, Raw: []byte(mustJSON(body))}
}

func started(activityID, toolUseID string) Received {
	return row(WireActivityStarted, activityID, toolUseID, map[string]any{"activity_type": "Bash"})
}

func completed(activityID, toolUseID string) Received {
	return row(WireActivityCompleted, activityID, toolUseID, map[string]any{"activity_type": "Bash"})
}

func ranCall(id string) Scenario {
	return Scenario{Payloads: []HookPayload{
		{Event: "PreToolUse", JSON: `{"session_id":"s","tool_name":"Bash","tool_use_id":"` + id + `"}`},
		{Event: "PostToolUse", JSON: `{"session_id":"s","tool_name":"Bash","tool_use_id":"` + id + `"}`},
	}}
}

func denied(id string) Scenario {
	sc := Scenario{Payloads: []HookPayload{
		{Event: "PreToolUse", JSON: `{"session_id":"s","tool_name":"Bash","tool_use_id":"` + id + `"}`},
	}}
	sc.Denied = map[string]bool{id: true}
	return sc
}

func decided(id, verb string) []Decision {
	return []Decision{{Event: "PreToolUse", ToolUseID: id, Verb: verb}}
}

func TestEveryGraderReasonIsReachable(t *testing.T) {
	const id, other = "toolu_a", "toolu_b"

	for _, tc := range []struct {
		name   string
		grader Grader
		sc     Scenario
		run    Run
		want   string
	}{
		{
			"pairing: a call's halves landed on two different rows",
			PairingGrader(), ranCall(id),
			Run{Inbox: []Received{started("act-1", id), completed("act-2", id)}},
			"different activity_ids",
		},
		{
			"pairing: declared blocked but the binary let it proceed",
			PairingGrader(), denied(id),
			Run{Inbox: []Received{started("act-1", id)}, Decisions: decided(id, "")},
			"but the binary rendered",
		},
		{
			"pairing: the binary denied a call the scenario does not declare",
			PairingGrader(), ranCall(id),
			Run{Inbox: []Received{started("act-1", id), completed("act-1", id)}, Decisions: decided(id, "deny")},
			"does not list it as blocked",
		},
		{
			"pairing: blocked before it ran, yet a completion arrived",
			PairingGrader(), denied(id),
			Run{Inbox: []Received{started("act-1", id), completed("act-1", id)}, Decisions: decided(id, "deny")},
			"yet 1 ActivityCompleted",
		},
		{
			"pairing: blocked, and reported more than once",
			PairingGrader(), denied(id),
			Run{Inbox: []Received{started("act-1", id), started("act-1", id)}, Decisions: decided(id, "deny")},
			"must be reported exactly once",
		},
		{
			"pairing: a call that ran is missing a half",
			PairingGrader(), ranCall(id),
			Run{Inbox: []Received{started("act-1", id)}},
			"a call that ran is reported exactly once each",
		},
		{
			"pairing: an activity row with no tool_use_id that is not a model turn",
			PairingGrader(), ranCall(id),
			Run{Inbox: []Received{row(WireActivityStarted, "act-9", "", map[string]any{"activity_type": "Bash"})}},
			"no metadata.tool_use_id on the wire",
		},
		{
			"pairing: a model turn that is not a pair",
			PairingGrader(), Scenario{},
			Run{Inbox: []Received{row(WireActivityStarted, "s:turn:1", "", map[string]any{"activity_type": activityTypeLLMCompletion})}},
			"a turn is one pair",
		},
		{
			"completeness: a demanded row never arrived",
			CompletenessGrader(), ranCall(id),
			Run{Inbox: []Received{started("act-1", id)}},
			"and none was delivered",
		},
		{
			"completeness: a row arrived that no input asked for",
			CompletenessGrader(), ranCall(id),
			Run{Inbox: []Received{started("act-1", id), completed("act-1", id), completed("act-2", other)}},
			"delivered with nothing in the input asking for it",
		},
		{
			"completeness: a demanded signal row never arrived",
			CompletenessGrader(),
			Scenario{Payloads: []HookPayload{{Event: "UserPromptSubmit", JSON: `{"session_id":"s","prompt":"hi"}`}}},
			Run{},
			"named prompt_submitted",
		},
		{
			"activity-type: a call is labelled as a different tool",
			ActivityTypeGrader(), ranCall(id),
			Run{Inbox: []Received{row(WireActivityStarted, "act-1", id, map[string]any{"activity_type": "Read"})}},
			"is labelled",
		},
		{
			"delivery-once: one call, two rows of the same kind",
			DeliveryOnceGrader(), ranCall(id),
			Run{Inbox: []Received{started("act-1", id), started("act-1", id)}},
			"was delivered 2 ActivityStarted rows",
		},
		{
			"signal-args: no prompt reached the wire at all",
			SignalArgsGrader("hello"), Scenario{},
			Run{Inbox: []Received{started("act-1", id)}},
			"no prompt_submitted signal reached the wire",
		},
		{
			"signal-args: capture is off but the prompt egressed anyway",
			SignalArgsGrader("hello"), Scenario{Posture: Posture{ContentCapture: "0"}},
			Run{Inbox: []Received{signalRow("prompt_submitted", map[string]any{"prompt": "hello"})}},
			"carries the prompt text",
		},
		{
			"signal-args: another signal class lost its payload",
			SignalArgsGrader("hello"), Scenario{Posture: Posture{ContentCapture: "1"}},
			Run{Inbox: []Received{
				signalRow("prompt_submitted", map[string]any{"prompt": "hello"}),
				row(WireSignalReceived, "", "", map[string]any{"signal_name": "notification"}),
			}},
			"carries no signal_args",
		},
		{
			"content-gate: capture is off but a row carried the canary",
			ContentGateGrader("CANARY"), Scenario{Posture: Posture{ContentCapture: "0"}},
			Run{Inbox: []Received{row(WireActivityStarted, "act-1", id, map[string]any{"activity_input": map[string]any{"command": "CANARY"}})}},
			"content capture is off but a",
		},
		{
			"content-gate: the canary landed outside every content field",
			ContentGateGrader("CANARY"), Scenario{Posture: Posture{ContentCapture: "1"}},
			Run{Inbox: []Received{
				row(WireActivityStarted, "act-1", id, map[string]any{"activity_input": map[string]any{"command": "CANARY"}}),
				row(WireActivityCompleted, "act-1", id, map[string]any{"note": "CANARY"}),
			}},
			"outside",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reasons := tc.grader.Check(tc.sc, tc.run)
			if len(reasons) == 0 {
				t.Fatalf("grader %q returned no reason; this branch cannot fire", tc.grader.Name)
			}
			if !strings.Contains(strings.Join(reasons, " | "), tc.want) {
				t.Errorf("no reason mentions %q; got %v", tc.want, reasons)
			}
		})
	}
}

func signalRow(name string, args map[string]any) Received {
	return row(WireSignalReceived, "", "", map[string]any{"signal_name": name, "signal_args": args})
}

func mustJSON(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestTheReferenceGradersWork keeps the two wrong answers trustworthy.
//
// They are controls: the suite asserts that parity rejects a healthy blocked
// session and that blanket exemption accepts a dropped completion. A control
// that is silently broken stops disagreeing with the real grader, and the suite
// reads that as the fixtures still exercising the distinction. So each is shown
// doing the thing it exists to do.
func TestTheReferenceGradersWork(t *testing.T) {
	t.Run("parity rejects an odd number of activity rows", func(t *testing.T) {
		run := Run{Inbox: []Received{started("act-1", "toolu_a")}}
		if len(ParityPairing().Check(Scenario{}, run)) == 0 {
			t.Error("the parity control accepted an odd total; it can no longer disagree with the real grader")
		}
	})
	t.Run("parity accepts an even number", func(t *testing.T) {
		run := Run{Inbox: []Received{started("act-1", "toolu_a"), completed("act-1", "toolu_a")}}
		if reasons := ParityPairing().Check(Scenario{}, run); len(reasons) != 0 {
			t.Errorf("the parity control rejected a paired call: %v", reasons)
		}
	})
	t.Run("blanket exemption still names an orphan completion", func(t *testing.T) {
		run := Run{Inbox: []Received{completed("act-1", "toolu_a")}}
		reasons := ExemptEverySingle().Check(Scenario{}, run)
		if len(reasons) == 0 {
			t.Error("the exempt-all control accepted a completion with no start; it now accepts everything and proves nothing")
		}
	})
	t.Run("blanket exemption accepts a lost completion, which is why it is wrong", func(t *testing.T) {
		run := Run{Inbox: []Received{started("act-1", "toolu_a")}}
		if reasons := ExemptEverySingle().Check(Scenario{}, run); len(reasons) != 0 {
			t.Errorf("the exempt-all control rejected a single-sided start, so it is no longer the wrong answer: %v", reasons)
		}
	})
}

// graderSources are the files whose reasons the table above must cover.
var graderSources = []string{"pairing.go", "graders.go", "contentgrader.go"}

// reasonSites is the number of places the graders can emit a reason, pinned.
//
// Pinned rather than compared against the table's length, because the two are
// not one-to-one: a single case can make several reasons fire, and some
// reasons share a message. A loose comparison looked like a guard and was not
// -- it had enough slack to absorb a new reason silently, which is the exact
// defect this file exists to close.
//
// So the number is pinned and any change to it is deliberate. Adding a reason
// fails here; the fix is to add a case to the table above that makes the new
// reason fire, then update this number. Deleting a reason fails here too, which
// is the point: it is a prompt to delete its case as well.
const reasonSites = 22

// TestEveryReasonSiteHasACase is the tripwire for the defect class this file
// exists to close.
//
// A grader that gains a reason nobody exercises looks exactly like one that
// works: the mutation meta-test still passes, because it only ever proves ONE
// route to red per grader. Sixteen reasons were shipped that way.
func TestEveryReasonSiteHasACase(t *testing.T) {
	sites := 0
	for _, f := range graderSources {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(trimmed, "reasons = append(reasons,") || strings.Contains(trimmed, `return []string{"`) {
				sites++
			}
		}
	}
	if sites == 0 {
		t.Fatal("found no reason-emitting sites at all; the scan is broken and this guard would pass vacuously")
	}
	if sites != reasonSites {
		t.Errorf("the graders can emit %d reasons, and %d were accounted for. "+
			"A reason nobody can trigger reads exactly like one that works, and the mutation meta-test will not "+
			"catch it. Add a case to TestEveryGraderReasonIsReachable that makes the new reason fire (or delete "+
			"the case for a reason that is gone), then update reasonSites to %d.", sites, reasonSites, sites)
	}
}
