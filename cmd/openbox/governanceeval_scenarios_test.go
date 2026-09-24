package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/client/workloadauth"
	"github.com/openbox-ai/openbox-shift-left/internal/conformance"
)

const (
	evalSession = "eval-s1"
	okToolUseID = "toolu_01ranfine"
	noToolUseID = "toolu_01blocked"
)

func hook(event, body string) fakecore.HookPayload {
	return fakecore.HookPayload{Event: event, JSON: body}
}

func sessionStart() fakecore.HookPayload {
	return hook("SessionStart", `{"hook_event_name":"SessionStart","session_id":"`+evalSession+`","cwd":"/repo","source":"startup"}`)
}

func userPrompt(text string) fakecore.HookPayload {
	return hook("UserPromptSubmit", `{"hook_event_name":"UserPromptSubmit","session_id":"`+evalSession+`","cwd":"/repo","prompt":"`+text+`"}`)
}

func sessionEnd() fakecore.HookPayload {
	return hook("SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"`+evalSession+`","cwd":"/repo","reason":"other"}`)
}

func preBash(toolUseID, command string) fakecore.HookPayload {
	return hook("PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"`+evalSession+`","cwd":"/repo","tool_name":"Bash","tool_use_id":"`+toolUseID+`","tool_input":{"command":"`+command+`"}}`)
}

func postBash(toolUseID, command string) fakecore.HookPayload {
	return hook("PostToolUse", `{"hook_event_name":"PostToolUse","session_id":"`+evalSession+`","cwd":"/repo","tool_name":"Bash","tool_use_id":"`+toolUseID+`","tool_input":{"command":"`+command+`"},"tool_response":{"stdout":"ok"}}`)
}

// authoredProvenance says plainly that these payloads were written, not
// captured. Recording a real session is an upgrade a later phase makes;
// labelling an authored fixture as recorded would be the lie that matters.
const authoredProvenance = "authored 2026-09-14 from the native shapes in cmd/openbox/main_test.go; not recorded from a live session"

// ranFineSession is the control: one Bash call that was allowed and completed.
func ranFineSession() fakecore.Scenario {
	return fakecore.Scenario{
		Name: "a-tool-that-ran-is-reported-as-a-pair",
		Payloads: []fakecore.HookPayload{
			sessionStart(),
			userPrompt("list the files"),
			preBash(okToolUseID, "ls -la"),
			postBash(okToolUseID, "ls -la"),
			sessionEnd(),
		},
		Provenance: authoredProvenance,
	}
}

// blockedSession is the case parity gets wrong: the call was denied at
// PreToolUse, so it never ran and no PostToolUse could fire. One row is the
// correct shape; a fabricated completion would be worse than the asymmetry.
func blockedSession() fakecore.Scenario {
	return fakecore.Scenario{
		Name: "a-blocked-tool-is-reported-once-not-twice",
		Payloads: []fakecore.HookPayload{
			sessionStart(),
			userPrompt("delete everything"),
			preBash(noToolUseID, "rm -rf /important"),
			sessionEnd(),
		},
		Verdicts: map[string]string{
			noToolUseID: `{"governance_event_id":"ge","verdict":"block","reason":"policy forbids recursive deletion","action":"block","policy_id":"p-1"}`,
		},
		Denied:     map[string]bool{noToolUseID: true},
		Provenance: authoredProvenance,
	}
}

// retriedSession repeats one command under two invocation ids. The two calls
// share an activity_id by design -- it is derived from the arguments alone, so
// an approved request can be consumed by its retry -- which is why pairing is
// keyed per invocation. Counted per activity_id this healthy session reads as
// a double delivery.
func retriedSession() fakecore.Scenario {
	const firstTry, secondTry = "toolu_01try1", "toolu_01try2"
	const cmd = "git status"
	return fakecore.Scenario{
		Name: "the-same-command-twice-is-two-calls-not-one-doubled",
		Payloads: []fakecore.HookPayload{
			sessionStart(),
			preBash(firstTry, cmd),
			postBash(firstTry, cmd),
			preBash(secondTry, cmd),
			postBash(secondTry, cmd),
			sessionEnd(),
		},
		Provenance: authoredProvenance,
	}
}

// observedSession runs with the gate off. Under enforce a gated PreToolUse
// egresses synchronously and its observe copy is discarded, so ToolCall and
// PromptSubmitted never reach the spool -- and the DevEvent validator would
// never see them. This is the scenario that puts them under it.
func observedSession() fakecore.Scenario {
	sc := ranFineSession()
	sc.Name = "an-unenforced-session-still-reports-every-call"
	sc.Posture = fakecore.Posture{Enforce: "0"}
	return sc
}

func TestGovernanceEval(t *testing.T) {
	for _, sc := range []fakecore.Scenario{
		ranFineSession(),
		blockedSession(),
		retriedSession(),
		observedSession(),
	} {
		t.Run(sc.Name, func(t *testing.T) {
			run := runScenario(t, sc)
			if len(run.Inbox) == 0 {
				t.Fatal("nothing reached the fake; the flush did not deliver")
			}
			grade(t, fakecore.PairingGrader(), sc, run.Run)
		})
	}
}

// TestGovernanceEvalDroppedCompletionIsNamed: losing a PostToolUse must be
// caught, and the reason must say which call lost it.
func TestGovernanceEvalDroppedCompletionIsNamed(t *testing.T) {
	sc := fakecore.Drop(ranFineSession(), "PostToolUse")
	run := runScenario(t, sc)
	reasons := fakecore.PairingGrader().Check(sc, run.Run)
	if len(reasons) != 1 {
		t.Fatalf("dropping the completion must produce exactly one named reason; got %d: %v", len(reasons), reasons)
	}
	if !strings.Contains(reasons[0], okToolUseID) {
		t.Errorf("the reason must name the call it is about; got %q", reasons[0])
	}
}

// TestGovernanceEvalTheWitnessMustBeCorroborated: the scenario's Denied set is
// a claim about the binary, not a licence. It has to agree with the decision
// the binary actually rendered, so a wrong declaration fails instead of
// silencing the call it names.
func TestGovernanceEvalTheWitnessMustBeCorroborated(t *testing.T) {
	t.Run("declaring-a-call-blocked-that-was-not", func(t *testing.T) {
		sc := ranFineSession() // scripted allow; the binary will not deny
		sc.Denied = map[string]bool{okToolUseID: true}
		run := runScenario(t, sc)
		reasons := fakecore.PairingGrader().Check(sc, run.Run)
		if len(reasons) == 0 {
			t.Fatal("a false denial claim passed; the witness is a silencer rather than an assertion")
		}
		if !strings.Contains(strings.Join(reasons, " "), "rendered") {
			t.Errorf("the reason should say what the binary actually rendered; got %v", reasons)
		}
	})

	t.Run("omitting-a-call-the-binary-did-block", func(t *testing.T) {
		sc := blockedSession()
		sc.Denied = nil // the binary still denies; the scenario no longer says so
		run := runScenario(t, sc)
		if len(fakecore.PairingGrader().Check(sc, run.Run)) == 0 {
			t.Error("with the witness removed the grader accepted a single-sided start, so it is exempting every single-sided start and a dropped completion would pass too")
		}
	})
}

// TestGovernanceEvalReferenceGraders keeps the two wrong answers executable.
// They are the proof that the fixtures still exercise the distinction the real
// grader is built on: if a fixture change made the blocked and dropped cases
// indistinguishable, the real grader could stay green while these stopped
// disagreeing with it.
func TestGovernanceEvalReferenceGraders(t *testing.T) {
	blocked := blockedSession()
	blockedRun := runScenario(t, blocked)
	if reasons := fakecore.ParityPairing().Check(blocked, blockedRun.Run); len(reasons) == 0 {
		t.Error("a parity check accepted the blocked session; the fixture no longer exercises the odd-total case that makes parity wrong")
	}

	dropped := fakecore.Drop(ranFineSession(), "PostToolUse")
	droppedRun := runScenario(t, dropped)
	if reasons := fakecore.ExemptEverySingle().Check(dropped, droppedRun.Run); len(reasons) != 0 {
		t.Errorf("exempting every single-sided start rejected the dropped-completion session; the fixture no longer exercises the case that makes blanket exemption wrong: %v", reasons)
	}
}

// TestGovernanceEvalMutations executes every grader's declared mutation and
// requires the named grader to go red, on a run that is otherwise healthy.
//
// This single test replaces three weaker devices: a minimum registry size, a
// mutation drill written up in a pull request, and a prose review rule. It
// also catches a grader that returns nil early, and one that is red on
// everything.
//
// Each grader brings its own base scenario. A content grader has nothing to
// say about a session carrying no content, and registering every grader
// against one shared base would make some of their mutations no-ops that still
// looked executed.
func TestGovernanceEvalMutations(t *testing.T) {
	registry := gradedScenarios()
	if len(registry) == 0 {
		t.Fatal("the grader registry is empty")
	}
	seen := map[string]bool{}
	for _, gs := range registry {
		if seen[gs.grader.Name] {
			t.Fatalf("two graders are registered as %q; one would silently shadow the other", gs.grader.Name)
		}
		seen[gs.grader.Name] = true
	}

	for _, gs := range registry {
		t.Run(gs.grader.Name, func(t *testing.T) {
			g := gs.grader
			if g.Mutate == nil {
				if gs.whyNoMutation == "" {
					t.Fatalf("grader %q declares no mutation and no reason, so nothing proves it can fail", g.Name)
				}
				t.Logf("no input mutation can falsify %q: %s", g.Name, gs.whyNoMutation)
				return
			}
			base := gs.base()
			if reasons := g.Check(base, runScenario(t, base).Run); len(reasons) != 0 {
				t.Fatalf("grader %q is red on its own healthy control, so its mutation proves nothing: %v", g.Name, reasons)
			}

			mutated, touched := g.Mutate(gs.base())
			run := runScenario(t, mutated)
			// The mutated run must still be a working session, or every
			// grader would be red for the wrong reason.
			requireHealthyRun(t, run)

			reasons := g.Check(mutated, run.Run)
			if len(reasons) == 0 {
				t.Fatalf("grader %q stayed green on its own declared mutation (%s); it cannot fail", g.Name, mutated.Name)
			}
			for _, r := range reasons {
				if strings.TrimSpace(r) == "" {
					t.Errorf("grader %q returned an empty reason; a reason nobody can read is a boolean", g.Name)
				}
			}
			// Named attribution, not isolation: dropping a completion
			// legitimately fails pairing AND completeness. What a grader may
			// not do is go red without saying which call it is about -- and it
			// can only name one by having found it.
			joined := strings.Join(reasons, " ")
			for _, id := range touched {
				if !strings.Contains(joined, id) {
					t.Errorf("grader %q went red but never named %s, the call its mutation interfered with: %v", g.Name, id, reasons)
				}
			}
		})
	}
}

// requireHealthyRun rejects a mutated run that broke the session rather than
// the property. Without this a mutation that dropped SessionEnd would turn
// every grader red and the meta-test would call that success.
func requireHealthyRun(t *testing.T, run evalRun) {
	t.Helper()
	for i, e := range run.Stderr {
		if strings.Contains(e, "recovered from panic") {
			t.Fatalf("payload #%d panicked during the mutated run: %s", i, e)
		}
	}
	var starts, ends int
	for _, r := range run.Inbox {
		switch r.EventType() {
		case fakecore.WireWorkflowStarted:
			starts++
		case fakecore.WireWorkflowCompleted:
			ends++
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("the mutated run delivered %d WorkflowStarted and %d WorkflowCompleted rows; the session itself is broken, so a red grader says nothing about the property", starts, ends)
	}
}

// TestGovernanceEvalSpoolSeesToolEvents: the DevEvent validator must actually
// reach tool events. Under enforce it cannot -- a gated PreToolUse egresses at
// the gate and its observe copy is discarded -- so this pins the unenforced
// scenario as the one that covers them. Without it AC2 would be half vacuous
// for the two most important types and nothing would say so.
func TestGovernanceEvalSpoolSeesToolEvents(t *testing.T) {
	trimmed := func(sc fakecore.Scenario) fakecore.Scenario {
		sc.Payloads = sc.Payloads[:len(sc.Payloads)-1] // stop before the flush drains it
		return sc
	}

	observed := fakecore.SpooledEventTypes(runScenario(t, trimmed(observedSession())).Spool)
	for _, want := range []string{"ToolCall", "ToolResult", "PromptSubmitted"} {
		if observed[want] == 0 {
			t.Errorf("no %s reached the spool with the gate off, so ValidateDevEvent never saw one; spooled: %v", want, observed)
		}
	}

	// And the enforced session cannot stand in for it: a gated call egresses
	// at the gate and its observe copy is discarded. If this ever starts
	// spooling them the unenforced scenario is redundant and should go --
	// but until then, dropping it would quietly un-cover two types.
	enforced := fakecore.SpooledEventTypes(runScenario(t, trimmed(ranFineSession())).Spool)
	for _, gated := range []string{"ToolCall", "PromptSubmitted"} {
		if enforced[gated] != 0 {
			t.Errorf("an enforced session spooled %s; the unenforced scenario is no longer the only thing covering it: %v", gated, enforced)
		}
	}
}

// TestGovernanceEvalRejectsAWrongKey: a body signed with a key core does not
// know is refused. Single-attempt delivery has no carry-over any more: a
// 401 -- which core answers both for a bad key and for a fault of its own --
// still spends the event's one attempt like every other class, so it is
// ledgered and gone, never left sitting in the spool for a retry that no
// longer exists.
func TestGovernanceEvalRejectsAWrongKey(t *testing.T) {
	sc := ranFineSession()
	fake := fakecore.New(t, sc.Script())

	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	evalEnv(t, fake, dir, spool, sc.Posture)
	// A throwaway key the fake has never seen. Generated here rather than
	// committed: a fixture seed shared with the signer would let a broken
	// signer pass. The bootstrap/exchange dance still succeeds (it verifies
	// only the API key), so what actually gets refused is the ASSERTION,
	// signed with a key whose JWK thumbprint core's Keycloak never registered.
	wrongKey, err := workloadauth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	wrongKeyB64, err := workloadauth.EncodePrivateKey(wrongKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(devconfig.EnvWorkloadPrivateKey, wrongKeyB64)

	for _, p := range sc.Payloads {
		a, _, _ := testApp(nil)
		a.stdin = strings.NewReader(p.JSON)
		a.run([]string{"hook", "claude-code", p.Event})
	}

	if n := len(fake.Inbox()); n != 0 {
		t.Errorf("the fake accepted %d wrongly-signed request(s); it verifies nothing", n)
	}
	if fake.ExchangeHits() == 0 {
		t.Fatal("nothing reached the token endpoint at all, so the rejection proves nothing")
	}
	if fake.V3EvaluateAttempts() != 0 {
		t.Errorf("a failed token exchange must never reach /evaluate at all; got %d attempts", fake.V3EvaluateAttempts())
	}
	if spoolStillHolds(t, spool) {
		t.Error("single-attempt delivery has no carry-over: every rejected event should be ledgered and gone, not left sitting in the spool")
	}
	if got := (hookflow.Spool{Dir: spool}).DiscardedCount(); got == 0 {
		t.Error("a token-acquisition failure still spends the event's one attempt and must be recorded in the discard ledger")
	}
}

// TestGovernanceEvalTwoObjectsTwoValidators: the DevEvent validator reads the
// SPOOL and the wire predicates read the wire, and neither is reachable by the
// other's object. Pointing ValidateDevEvent at a wire body would reject every
// event, after which the natural fix is to soft-fail it into a no-op.
func TestGovernanceEvalTwoObjectsTwoValidators(t *testing.T) {
	const wireBody = `{"source":"developer-runtime","event_type":"ActivityStarted","workflow_id":"w","run_id":"r","activity_id":"a","timestamp":"2026-09-14T00:00:00Z"}`

	if err := conformance.ValidateDevEvent([]byte(`{"event_type":"tool_call"}`), false); err == nil {
		t.Error("a DevEvent missing every required field validated; the validator is a no-op")
	}
	if err := conformance.ValidateDevEvent([]byte(wireBody), false); err == nil {
		t.Error("a wire payload validated as a DevEvent; the two objects have been conflated")
	}
	if reasons := fakecore.CheckWireShape([]byte(wireBody)); len(reasons) != 0 {
		t.Errorf("a well-formed wire body was rejected by the wire predicates: %v", reasons)
	}
}

func spoolStillHolds(t *testing.T, spool string) bool {
	t.Helper()
	entries, err := os.ReadDir(spool)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(spool, e.Name()))
		if len(strings.TrimSpace(string(b))) > 0 {
			return true
		}
	}
	return false
}
