package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
)

// The verdict loop, each branch asserted at the effect a regression would
// actually break.
//
// Two of the five cannot be graded on stdout at all:
//
//   - CONSTRAIN renders exactly what ALLOW renders -- nothing. MapVerdict
//     falls through, so a stdout assertion for it passes whether the verdict
//     arrived or not. Its only local effect is the constraints field in the
//     enforcement ledger, so that is what is graded.
//   - REQUIRE_APPROVAL never renders "ask" through this path. The hold runs
//     before the decision is applied, and an unanswered request is rewritten
//     to a HALT, which renders deny. Asserting "ask" would assert a branch the
//     gate cannot reach.
//
// The two that look alike are separated by what they leave on disk: a HALT
// latches the session, an unanswered approval does not -- it files a pending
// marker instead. That pair is the strongest evidence here, because it is the
// difference between "the org stopped you" and "someone has not answered yet".

const (
	allowVerdict     = `{"governance_event_id":"ge","verdict":"allow"}`
	constrainVerdict = `{"governance_event_id":"ge","verdict":"constrain","reason":"scoped to the repo","constraints":[{"type":"path","value":"/repo"}]}`
	blockVerdict     = `{"governance_event_id":"ge","verdict":"block","reason":"policy forbids recursive deletion","policy_id":"p-block"}`
	haltVerdict      = `{"governance_event_id":"ge","verdict":"halt","reason":"org kill switch","policy_id":"p-halt"}`
	approvalVerdict  = `{"governance_event_id":"ge","verdict":"require_approval","reason":"a human must say yes","approval_ref":"ar-1"}`
)

// oneCallSession is a session whose single tool call carries the verdict under
// test, so each branch is graded in isolation.
func oneCallSession(name, verdict string, denied bool) fakecore.Scenario {
	sc := fakecore.Scenario{
		Name: name,
		Payloads: []fakecore.HookPayload{
			sessionStart(),
			preBash(noToolUseID, "rm -rf /important"),
			sessionEnd(),
		},
		Verdicts:   map[string]string{noToolUseID: verdict},
		Provenance: authoredProvenance,
	}
	if denied {
		sc.Denied = map[string]bool{noToolUseID: true}
	} else {
		// The call proceeded, so the provider would have fired PostToolUse.
		sc.Payloads = []fakecore.HookPayload{
			sessionStart(),
			preBash(noToolUseID, "rm -rf /important"),
			postBash(noToolUseID, "rm -rf /important"),
			sessionEnd(),
		}
	}
	return sc
}

func TestGovernanceEvalVerdictBranches(t *testing.T) {
	t.Run("an-allowed-call-proceeds-silently", func(t *testing.T) {
		sc := oneCallSession("allow", allowVerdict, false)
		run := runScenario(t, sc)
		requireVerb(t, run, "")
		requireLedger(t, run, "ALLOW", map[string]any{"source": "evaluate", "would_block": false})
		requireNoHaltLatch(t, run)
	})

	t.Run("a-constrained-call-proceeds-and-the-constraint-is-recorded", func(t *testing.T) {
		sc := oneCallSession("constrain", constrainVerdict, false)
		run := runScenario(t, sc)
		// Identical to allow. Asserting it would be asserting nothing.
		requireVerb(t, run, "")
		rec := requireLedger(t, run, "CONSTRAIN", nil)
		cs, ok := rec["constraints"].([]any)
		if !ok || len(cs) == 0 {
			t.Fatalf("the ledger records no constraints, which is the verdict's only local effect: %v", rec)
		}
	})

	t.Run("a-blocked-call-is-denied-to-the-coding-agent", func(t *testing.T) {
		sc := oneCallSession("block", blockVerdict, true)
		run := runScenario(t, sc)
		d := requireVerb(t, run, "deny")
		if d.Stop {
			t.Error("a BLOCK stopped the session; only a HALT may do that")
		}
		requireLedger(t, run, "BLOCK", map[string]any{"applied_decision": "deny", "would_block": true})
		requireNoHaltLatch(t, run)
		grade(t, fakecore.PairingGrader(), sc, run.Run)
	})

	t.Run("a-halt-denies-the-call-and-latches-the-session", func(t *testing.T) {
		sc := oneCallSession("halt", haltVerdict, true)
		run := runScenario(t, sc)
		d := requireVerb(t, run, "deny")
		if !d.Stop {
			t.Error("a HALT did not stop the session; the latch and the stop lever are the whole difference from a BLOCK")
		}
		requireLedger(t, run, "HALT", map[string]any{"applied_decision": "halt"})
		if n := countFiles(t, filepath.Join(run.Dir, "halts")); n == 0 {
			t.Error("a HALT wrote no session latch, so the next call in this run would be evaluated fresh")
		}
	})
}

// TestGovernanceEvalApproval covers the hold in both directions. The negative
// alone is worthless: with no approval endpoint the route 404s, the hold reads
// that as undecided, and "an unanswered approval denies" passes through the
// degraded path without the poll response ever being parsed. The granted case
// is what proves the answer is read.
func TestGovernanceEvalApproval(t *testing.T) {
	t.Run("an-unanswered-approval-denies", func(t *testing.T) {
		sc := oneCallSession("unanswered-approval", approvalVerdict, true)
		sc.Posture.ApprovalHoldMS = "300"
		sc.Approval = func(fakecore.Received) (int, string) {
			// Filed, still pending: the shape an approver has not reached yet.
			return 200, `{"id":"ge","action":"require_approval","approval_expiration_time":"` + future() + `"}`
		}
		run := runScenario(t, sc)

		if run.Fake.ApprovalPolls() == 0 {
			t.Fatal("the approval endpoint was never polled; the deny came from somewhere other than the hold")
		}
		requireVerb(t, run, "deny")
		rec := requireLedger(t, run, "HALT", map[string]any{"source": "approval:undecided"})
		_ = rec
		// The marker that separates a real hold from fail-closed's deny.
		if n := countFiles(t, filepath.Join(run.Dir, "pending-approvals")); n == 0 {
			t.Error("no pending-approval marker was filed, so nothing records what is being waited on")
		}
		// An unanswered approval is not an org kill switch: it denies this one
		// call and leaves the session usable.
		requireNoHaltLatch(t, run)
	})

	// The grant and the rejection together prove the poll ANSWER is read. With
	// only the unanswered case, a hold that never parsed the response would
	// still deny and still pass.
	t.Run("a-rejected-request-is-denied", func(t *testing.T) {
		sc := oneCallSession("rejected-approval", approvalVerdict, true)
		sc.Posture.ApprovalHoldMS = "3000"
		sc.Approval = func(fakecore.Received) (int, string) {
			return 200, `{"id":"ge","action":"block","reason":"a human said no","approval_expiration_time":"` + future() + `"}`
		}
		run := runScenario(t, sc)
		if run.Fake.ApprovalPolls() == 0 {
			t.Fatal("the approval endpoint was never polled")
		}
		d := requireVerb(t, run, "deny")
		// A refusal a human made is not the same event as nobody answering,
		// and the ledger must not report it as one.
		rec := requireLedgerRow(t, run)
		if src, _ := rec["source"].(string); src == "approval:undecided" {
			t.Error("a decided rejection was recorded as undecided; the poll answer was not read, only its absence")
		}
		_ = d
	})

	t.Run("an-approved-request-proceeds", func(t *testing.T) {
		sc := oneCallSession("granted-approval", approvalVerdict, false)
		sc.Posture.ApprovalHoldMS = "3000"
		sc.Approval = func(fakecore.Received) (int, string) {
			return 200, `{"id":"ge","action":"allow","reason":"approved by a human","approval_expiration_time":"` + future() + `"}`
		}
		run := runScenario(t, sc)

		if run.Fake.ApprovalPolls() == 0 {
			t.Fatal("the approval endpoint was never polled")
		}
		if d := requireVerb(t, run, ""); d.Stop {
			t.Error("an approved request stopped the session")
		}
		rec := requireLedgerRow(t, run)
		if src, _ := rec["source"].(string); src != "approval:decided" {
			t.Errorf("the ledger records source %q, want approval:decided; proceeding is not the same as having read an answer", src)
		}
		grade(t, fakecore.PairingGrader(), sc, run.Run)
	})
}

// TestGovernanceEvalFailClosed covers the ordering invariant in both
// directions.
//
// The first case is the one that pins the ORDER rather than the occurrence. If
// the failure policy ran before the evaluation instead of after it, it would
// synthesize a HALT under fail_closed and suppress the round trip entirely --
// so every gated call would be denied without ever asking. A call that
// PROCEEDS under fail_closed is only reachable if /evaluate ran first and
// answered allow, and hits == 1 says the round trip is what it ran.
func TestGovernanceEvalFailClosed(t *testing.T) {
	t.Run("fail-closed-plus-allow-still-proceeds", func(t *testing.T) {
		sc := oneCallSession("fail-closed-allow", allowVerdict, false)
		sc.Posture.FailClosed = "1"
		run := runScenario(t, sc)
		// Proceeding at all is the ordering proof: had the policy run first it
		// would have synthesized a HALT and never asked. What remains to pin
		// is that the round trip happened and happened once -- counted as the
		// gated call's own delivered row, because Hits() also counts the
		// session's flush.
		requireVerb(t, run, "")
		if run.Fake.Hits() == 0 {
			t.Error("the call proceeded without /evaluate being reached at all")
		}
		if n := countStartedFor(run, noToolUseID); n != 1 {
			t.Errorf("the gated call reached the wire %d times, want exactly 1; under enforce the gate delivers it synchronously and the observe copy is discarded", n)
		}
	})

	// sc.AlwaysStatus applies to EVERY /evaluate POST, including SessionStart's
	// own inline WorkflowStarted delivery -- which now runs, and fails,
	// BEFORE the gated call ever escalates (SessionStart delivers inline,
	// and a failed WorkflowStarted halts the run like any other failure). So the run is already latched by the time the gated
	// call's own gate runs, and it denies via the latch replay without
	// asking /evaluate again -- not via its own failed escalation. This
	// supersedes the old ordering pin (the gated call's OWN escalation
	// failing, evidenced by source=evaluate:fail-open and hits>=1): the
	// FIRST failure a run hits, whichever event it belongs to, is now what
	// halts it.
	t.Run("fail-closed-plus-an-outage-denies", func(t *testing.T) {
		sc := oneCallSession("fail-closed-outage", allowVerdict, true)
		sc.Posture.FailClosed = "1"
		sc.AlwaysStatus = 500
		run := runScenario(t, sc)

		d := requireVerb(t, run, "deny")
		if run.Fake.Hits() < 1 {
			t.Error("nothing ever reached /evaluate at all")
		}
		rec := requireLedgerRow(t, run)
		if src, _ := rec["source"].(string); src != hookflow.SourceSessionHalt {
			t.Errorf("the ledger records source %q, want %q; SessionStart's own inline delivery failure "+
				"should have latched the run before the gated call ever escalated", src, hookflow.SourceSessionHalt)
		}
		if d.Reason == "" {
			t.Error("a fail-closed deny gave the coding agent no reason at all")
		}
		for _, leak := range []string{"rm -rf", "/important"} {
			if containsFold(d.Reason, leak) {
				t.Errorf("the fail-closed reason quoted the tool content (%q); it must be content-free: %q", leak, d.Reason)
			}
		}
		// An outage now IS a run-halting condition, the
		// opposite of the old "an outage is not a kill switch" pin.
		if n := countFiles(t, filepath.Join(run.Dir, "halts")); n != 1 {
			t.Errorf("%d session halt latch(es) were written, want exactly 1", n)
		}
	})
}

// countStartedFor counts the delivered ActivityStarted rows for one call.
func countStartedFor(run evalRun, toolUseID string) int {
	n := 0
	for _, r := range run.Inbox {
		if r.EventType() == fakecore.WireActivityStarted && r.ToolUseID() == toolUseID {
			n++
		}
	}
	return n
}

func requireVerb(t *testing.T, run evalRun, want string) fakecore.Decision {
	t.Helper()
	d, ok := run.DecisionFor(noToolUseID)
	if !ok {
		t.Fatalf("no decision was recorded for the gated call; it was never gated")
	}
	if d.Verb != want {
		t.Errorf("the coding agent read permissionDecision %q, want %q (reason %q)", d.Verb, want, d.Reason)
	}
	return d
}

// requireLedger finds the tool call's ledger row and checks the fields given.
// The prompt's own row is skipped: every scenario has one and it is never the
// subject.
func requireLedger(t *testing.T, run evalRun, wantVerdict string, fields map[string]any) map[string]any {
	t.Helper()
	for _, rec := range run.Ledger {
		if kind, _ := rec["tool_kind"].(string); kind == "prompt" {
			continue
		}
		if got, _ := rec["verdict"].(string); got != wantVerdict {
			continue
		}
		for k, want := range fields {
			if got := rec[k]; got != want {
				t.Errorf("ledger %s = %v, want %v (row: %v)", k, got, want, rec)
			}
		}
		return rec
	}
	t.Fatalf("no enforcement ledger row carries verdict %s; the decision was not recorded at all. rows: %v", wantVerdict, run.Ledger)
	return nil
}

// requireLedgerRow returns the tool call's ledger row whatever its verdict.
func requireLedgerRow(t *testing.T, run evalRun) map[string]any {
	t.Helper()
	for _, rec := range run.Ledger {
		if kind, _ := rec["tool_kind"].(string); kind != "prompt" {
			return rec
		}
	}
	t.Fatalf("the tool call left no enforcement ledger row: %v", run.Ledger)
	return nil
}

func requireNoHaltLatch(t *testing.T, run evalRun) {
	t.Helper()
	if n := countFiles(t, filepath.Join(run.Dir, "halts")); n != 0 {
		t.Errorf("%d session halt latch(es) were written; only a HALT the control plane actually returned may latch a session, or an outage would keep denying after it ended", n)
	}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			n++
		}
	}
	return n
}

func future() string { return time.Now().Add(time.Hour).UTC().Format(time.RFC3339) }

func containsFold(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		stringsContainsFold(haystack, needle)
}

// TestGovernanceEvalGateOutageReportsTheCallOnce: core is down only while
// the tool call is gated. The call is denied (fail-closed), but a 503 on the
// gate's own escalation is transient: it does not halt the run, and the
// call's observe copy is requeued instead. Once core is back, a drainer
// delivers that copy, so the call is recorded exactly once and no halt latch
// is written. PairingGrader is not run here: its "declaredDenied" contract
// expects the ActivityStarted row of a real BLOCK/HALT verdict, not a copy
// delivered after a denied escalation.
func TestGovernanceEvalGateOutageReportsTheCallOnce(t *testing.T) {
	sc := oneCallSession("a-gate-outage-halts-the-run", allowVerdict, true)
	// Down while the tool call is gated, back up by the time the session ends.
	sc.OutageDuring = func(_ int, event string) bool { return event == "PreToolUse" }
	run := runScenario(t, sc)

	if run.Fake.ScriptedFailures() == 0 {
		t.Fatal("the outage never happened, so the election was never forced")
	}
	d := requireVerb(t, run, "deny")
	if d.Reason == "" {
		t.Error("a fail-closed deny gave the coding agent no reason at all")
	}

	if n := countStartedFor(run, noToolUseID); n != 1 {
		t.Errorf("the call was recorded %d times, want exactly 1: its requeued observe copy, delivered once core was back", n)
	}
	if n := countFiles(t, filepath.Join(run.Dir, "halts")); n != 0 {
		t.Errorf("%d session halt latch(es) were written, want 0: a transient escalation failure that later delivers must not halt the run", n)
	}
}

// TestGovernanceEvalHaltLatchesTheRestOfTheRun is the behavioural half of the
// latch, and the real difference between a HALT and a BLOCK.
//
// A file under the halt directory is weak evidence: the directory is
// env-pinned, so a regression writing the latch somewhere else would satisfy a
// file check while the session carried on. What the latch is FOR is that the
// next call in the run is refused without asking again -- so that is what is
// asserted.
func TestGovernanceEvalHaltLatchesTheRestOfTheRun(t *testing.T) {
	const secondCall = "toolu_01after"
	sc := fakecore.Scenario{
		Name: "a-halt-refuses-the-next-call-without-asking-again",
		Payloads: []fakecore.HookPayload{
			sessionStart(),
			preBash(noToolUseID, "rm -rf /important"),
			preBash(secondCall, "ls -la"),
			sessionEnd(),
		},
		Verdicts: map[string]string{
			noToolUseID: haltVerdict,
			// Scripted ALLOW: if the latch is not consulted, this call
			// proceeds and the test says so.
			secondCall: allowVerdict,
		},
		Denied:     map[string]bool{noToolUseID: true, secondCall: true},
		Provenance: authoredProvenance,
	}
	run := runScenario(t, sc)

	first, _ := run.DecisionFor(noToolUseID)
	if !first.Stop {
		t.Fatal("the HALT did not stop the session, so there is no latch to test")
	}
	second, ok := run.DecisionFor(secondCall)
	if !ok {
		t.Fatal("the second call was never gated")
	}
	if second.Verb != "deny" {
		t.Errorf("the call after a HALT rendered %q; a latched run refuses every later call, and this one was scripted ALLOW so it would have proceeded had the latch not been read", second.Verb)
	}
	grade(t, fakecore.PairingGrader(), sc, run.Run)
}
