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
// captured. Recording a real session would be an upgrade;
// labelling an authored fixture as recorded would be the lie that matters.
const authoredProvenance = "authored from the native shapes in cmd/openbox/main_test.go; not recorded from a live session"

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

// gradeOrdering applies the two graders every ordered scenario in this suite
// is held to: no row of a run precedes that same run's own WorkflowStarted,
// and no event was attempted more than once beyond what the script itself
// held past a caller's own budget. startFirst is false only for a scenario
// that deliberately never fires SessionStart at all (the `claude -p`
// headless shape), for which there is no WorkflowStarted to precede.
func gradeOrdering(t *testing.T, sc fakecore.Scenario, run fakecore.Run, startFirst bool) {
	t.Helper()
	if startFirst {
		grade(t, fakecore.StartFirst(), sc, run)
	}
	grade(t, fakecore.OneAttempt(), sc, run)
}

// TestGovernanceEval: enforcement is unconditional now (ResolveEnforce always
// reports true), so there is no "unenforced" scenario left to run alongside
// these -- TestGovernanceEvalSpoolSeesToolEvents already covers the one thing
// that variant existed for (ToolCall/PromptSubmitted reaching the spool at
// all). Every scenario here is additionally held to StartFirst/OneAttempt:
// the claim that a fresh SessionStart -> PreToolUse -> PostToolUse ->
// SessionEnd session's first core row is WorkflowStarted is exactly
// ranFineSession's own shape, so it is proven here rather than duplicated as
// a second fixture.
func TestGovernanceEval(t *testing.T) {
	for _, sc := range []fakecore.Scenario{
		ranFineSession(),
		blockedSession(),
		retriedSession(),
	} {
		t.Run(sc.Name, func(t *testing.T) {
			run := runScenario(t, sc)
			if len(run.Inbox) == 0 {
				t.Fatal("nothing reached the fake; the flush did not deliver")
			}
			grade(t, fakecore.PairingGrader(), sc, run.Run)
			gradeOrdering(t, sc, run.Run, true)
		})
	}
}

// claudePrintSession models `claude -p` (headless, non-interactive mode): the
// run opens directly on the prompt, with no SessionStart hook ahead of it at
// all -- so there is no WorkflowStarted for anything in this run to precede,
// which is exactly why StartFirst is not asked about this one (see
// gradeOrdering's own doc comment).
func claudePrintSession() fakecore.Scenario {
	return fakecore.Scenario{
		Name: "claude-p-headless-mode-opens-on-the-prompt-with-no-session-start-ahead-of-it",
		Payloads: []fakecore.HookPayload{
			userPrompt("summarize this repository"),
			preBash(okToolUseID, "ls -la"),
			postBash(okToolUseID, "ls -la"),
			sessionEnd(),
		},
		Provenance: authoredProvenance,
	}
}

// TestGovernanceEvalHeadlessPromptOpensTheRun: `claude -p`'s own first row is
// the prompt itself, and OneAttempt still holds even with no SessionStart in
// the payload list.
func TestGovernanceEvalHeadlessPromptOpensTheRun(t *testing.T) {
	sc := claudePrintSession()
	run := runScenario(t, sc)
	if len(run.Inbox) == 0 {
		t.Fatal("nothing reached the fake; the flush did not deliver")
	}
	first := run.Inbox[0]
	if got := first.EventType(); got != fakecore.WireSignalReceived {
		t.Fatalf("first row = %q, want %q: headless mode has no SessionStart, so the prompt itself opens the run", got, fakecore.WireSignalReceived)
	}
	if name, _ := first.Body["signal_name"].(string); name != "prompt_submitted" {
		t.Fatalf("first row's signal_name = %q, want prompt_submitted", name)
	}
	grade(t, fakecore.PairingGrader(), sc, run.Run)
	gradeOrdering(t, sc, run.Run, false)
}

// Codex's own native hook shapes (turn_id, model, permission_mode; see
// internal/adapters/codex/testdata/*.json), for the Codex twin scenario below.

func codexSessionStart() fakecore.HookPayload {
	return hook("SessionStart", `{"hook_event_name":"SessionStart","session_id":"`+evalSession+`","cwd":"/repo","model":"gpt-5.3-codex","permission_mode":"default","source":"startup"}`)
}

func codexUserPrompt(text string) fakecore.HookPayload {
	return hook("UserPromptSubmit", `{"hook_event_name":"UserPromptSubmit","session_id":"`+evalSession+`","turn_id":"turn-1","cwd":"/repo","model":"gpt-5.3-codex","permission_mode":"default","prompt":"`+text+`"}`)
}

func codexPreBash(toolUseID, command string) fakecore.HookPayload {
	return hook("PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"`+evalSession+`","turn_id":"turn-1","cwd":"/repo","model":"gpt-5.3-codex","permission_mode":"default","tool_name":"Bash","tool_use_id":"`+toolUseID+`","tool_input":{"command":"`+command+`"}}`)
}

func codexPostBash(toolUseID, command string) fakecore.HookPayload {
	return hook("PostToolUse", `{"hook_event_name":"PostToolUse","session_id":"`+evalSession+`","turn_id":"turn-1","cwd":"/repo","model":"gpt-5.3-codex","permission_mode":"default","tool_name":"Bash","tool_use_id":"`+toolUseID+`","tool_input":{"command":"`+command+`"},"tool_response":{"output":"ok"}}`)
}

func codexSessionEnd() fakecore.HookPayload {
	return hook("SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"`+evalSession+`","cwd":"/repo","reason":"other"}`)
}

// codexTwinSession: ranFineSession's own shape, driven through `hook codex`
// instead of `hook claude-code` -- the same StartFirst/OneAttempt ordering
// claim must hold for the second provider, not only the first.
func codexTwinSession() fakecore.Scenario {
	return fakecore.Scenario{
		Name:     "codex-twin-of-the-ran-fine-session-orders-the-same-way",
		Provider: "codex",
		Payloads: []fakecore.HookPayload{
			codexSessionStart(),
			codexUserPrompt("list the files"),
			codexPreBash(okToolUseID, "ls -la"),
			codexPostBash(okToolUseID, "ls -la"),
			codexSessionEnd(),
		},
		Provenance: authoredProvenance,
	}
}

func TestGovernanceEvalCodexTwinOrdersTheSameWay(t *testing.T) {
	sc := codexTwinSession()
	run := runScenario(t, sc)
	if len(run.Inbox) == 0 {
		t.Fatal("nothing reached the fake; the flush did not deliver")
	}
	if got := run.Inbox[0].EventType(); got != fakecore.WireWorkflowStarted {
		t.Fatalf("first row = %q, want WorkflowStarted", got)
	}
	grade(t, fakecore.PairingGrader(), sc, run.Run)
	gradeOrdering(t, sc, run.Run, true)
}

// TestGovernanceEvalCoreDownAtSessionStartHaltsAndRecovers: SessionStart's
// own inline WorkflowStarted attempt fails while core is down, so the first
// gated call of that run denies with the delivery reason and every later
// gated call of the SAME run stays denied too -- OneAttempt (graded generically
// below) proves there is no redelivery once core recovers, because
// single-attempt delivery has no carry-over. A brand NEW session, started
// only after recovery, is unaffected: a different session id is a different
// (unlatched) run.
func TestGovernanceEvalCoreDownAtSessionStartHaltsAndRecovers(t *testing.T) {
	const haltedSession = "recovery-halted-session"
	const recoveredSession = "recovery-new-session"
	haltedStart := hook("SessionStart", `{"hook_event_name":"SessionStart","session_id":"`+haltedSession+`","cwd":"/repo","source":"startup"}`)
	haltedFirstCall := hook("PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"`+haltedSession+`","cwd":"/repo","tool_name":"Bash","tool_use_id":"toolu_before_halt_first","tool_input":{"command":"ls"}}`)
	haltedSecondCall := hook("PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"`+haltedSession+`","cwd":"/repo","tool_name":"Bash","tool_use_id":"toolu_before_halt_second","tool_input":{"command":"pwd"}}`)
	haltedEnd := hook("SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"`+haltedSession+`","cwd":"/repo","reason":"other"}`)
	recoveredStart := hook("SessionStart", `{"hook_event_name":"SessionStart","session_id":"`+recoveredSession+`","cwd":"/repo","source":"startup"}`)
	recoveredCall := hook("PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"`+recoveredSession+`","cwd":"/repo","tool_name":"Bash","tool_use_id":"toolu_after_recovery","tool_input":{"command":"ls"}}`)
	recoveredComplete := hook("PostToolUse", `{"hook_event_name":"PostToolUse","session_id":"`+recoveredSession+`","cwd":"/repo","tool_name":"Bash","tool_use_id":"toolu_after_recovery","tool_input":{"command":"ls"},"tool_response":{"stdout":"ok"}}`)
	recoveredEnd := hook("SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"`+recoveredSession+`","cwd":"/repo","reason":"other"}`)

	sc := fakecore.Scenario{
		Name: "core-down-at-session-start-halts-the-run-and-a-new-session-after-recovery-is-unaffected",
		Payloads: []fakecore.HookPayload{
			haltedStart, haltedFirstCall, haltedSecondCall, haltedEnd,
			recoveredStart, recoveredCall, recoveredComplete, recoveredEnd,
		},
		Denied: map[string]bool{"toolu_before_halt_first": true, "toolu_before_halt_second": true},
		// Down for the halted session's own three payloads (index 0-2; its
		// SessionEnd at index 3 is deliberately left down too, so its own
		// WorkflowCompleted attempt fails the same way and never un-halts
		// anything), back up from the new session's SessionStart onward.
		OutageDuring: func(i int, _ string) bool { return i <= 3 },
		Provenance:   authoredProvenance,
	}
	run := runScenario(t, sc)

	first, ok := run.DecisionFor("toolu_before_halt_first")
	if !ok || first.Verb != "deny" {
		t.Fatalf("the first gated call of the halted session = %+v, want a deny", first)
	}
	if !strings.Contains(first.Reason, "OpenBox could not record") {
		t.Errorf("the first call's deny reason does not name the delivery failure: %q", first.Reason)
	}
	second, ok := run.DecisionFor("toolu_before_halt_second")
	if !ok || second.Verb != "deny" {
		t.Fatalf("the second gated call of the SAME halted run = %+v, want a deny too", second)
	}

	recovered, ok := run.DecisionFor("toolu_after_recovery")
	if !ok || recovered.Verb != "" {
		t.Fatalf("the NEW session's own gated call = %+v, want a silent allow: a different session id is a different, unlatched run", recovered)
	}

	if n := len(fakecore.SpoolLines(run.Spool)); n != 0 {
		t.Errorf("%d line(s) remain spooled after the flush pass; single-attempt delivery has no carry-over", n)
	}
	grade(t, fakecore.HaltedAfterFailure(), sc, run.Run)
	// OneAttempt is the "no redelivery after recovery" proof: every
	// idempotency key this run used, including the halted session's own
	// failed WorkflowStarted, was attempted exactly once.
	gradeOrdering(t, sc, run.Run, true)
}

// deliveryFailureClass is one class of "core did not accept this event": what the
// fake is scripted to do, and how the test proves the ONE attempt actually
// happened.
type deliveryFailureClass struct {
	name string
	// alwaysStatus, when non-zero, becomes the scenario's own
	// Scenario.AlwaysStatus (a scripted refusal the wire-shape/auth layer
	// never reaches on its own); zero leaves the scenario's default (0, no
	// forced status).
	alwaysStatus int
	setup        func(t fakecore.TB, fake *fakecore.Server)
	// verify adds the class-specific proof for a class that never even
	// reaches /evaluate at all (so OneAttempt's own idempotency-key
	// bookkeeping has nothing to count for it): nil for a class the generic
	// AttemptsByKey-based OneAttempt grading already proves meaningfully.
	verify func(t *testing.T, fake *fakecore.Server)
}

// TestGovernanceEvalOneAttemptPerFailureClass: for every failure class
// this client can hit trying to reach /evaluate, the gated call is denied,
// the run is latched, and the NEXT gated call of the same run is denied too
// -- naming the same delivery failure -- all in exactly one attempt.
func TestGovernanceEvalOneAttemptPerFailureClass(t *testing.T) {
	const firstCall, secondCall = "toolu_before_halt", "toolu_after_halt"
	for _, tc := range []deliveryFailureClass{
		// A closed listener: nothing answers at all, so the client's very
		// first real dial (bootstrap, then the token exchange, then
		// /evaluate -- whichever it reaches first) gets a genuine connection
		// refusal, never a context deadline. That is what keeps this
		// "explicit, proven non-acceptance" rather than the gate's own
		// "unanswered" branch (a context timeout, which never halts).
		{
			name: "network",
			setup: func(tb fakecore.TB, _ *fakecore.Server) {
				tb.(*testing.T).Setenv(devconfig.EnvBaseURL, "http://127.0.0.1:1")
			},
			verify: func(t *testing.T, fake *fakecore.Server) {
				if n := fake.Hits(); n != 0 {
					t.Errorf("fakecore recorded %d hit(s); a closed-listener network failure must never reach it at all", n)
				}
			},
		},
		{name: "5xx", setup: func(_ fakecore.TB, fake *fakecore.Server) { fake.SetOutage(true) }},
		{name: "401", setup: func(_ fakecore.TB, fake *fakecore.Server) { fake.Revoke() }},
		// 422 is scripted through Scenario.AlwaysStatus (alwaysStatus
		// below), not through the server directly, so this class needs no
		// setup at all.
		{name: "4xx", alwaysStatus: 422},
		{
			name:  "token-exchange-failure",
			setup: func(_ fakecore.TB, fake *fakecore.Server) { fake.SetExchangeFailure(400, "invalid_client") },
			verify: func(t *testing.T, fake *fakecore.Server) {
				// A halted run's remaining events -- here, the two denied
				// calls' own observe copies plus SessionEnd's own
				// WorkflowCompleted -- still each get their own one
				// delivery attempt (failures there do not change the
				// latch), and every one of THOSE independently
				// re-attempts the exchange too (nothing ever caches a
				// valid token), so ExchangeHits is not pinned to 1 here;
				// what IS pinned is that a failure at this stage never
				// reaches /evaluate at all.
				if n := fake.ExchangeHits(); n == 0 {
					t.Error("the token exchange was never attempted at all, so this class was never actually exercised")
				}
				if n := fake.V3EvaluateAttempts(); n != 0 {
					t.Errorf("a failed token exchange must never reach /evaluate at all; got %d attempt(s)", n)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := fakecore.Scenario{
				Name: "the-" + tc.name + "-class-halts-the-run-in-exactly-one-attempt",
				Payloads: []fakecore.HookPayload{
					sessionStart(),
					preBash(firstCall, "ls"),
					preBash(secondCall, "pwd"),
					sessionEnd(),
				},
				Denied:       map[string]bool{firstCall: true, secondCall: true},
				AlwaysStatus: tc.alwaysStatus,
				Setup:        tc.setup,
				Provenance:   authoredProvenance,
			}
			run := runScenario(t, sc)

			first, ok := run.DecisionFor(firstCall)
			if !ok || first.Verb != "deny" {
				t.Fatalf("first call = %+v, want a deny", first)
			}
			if !strings.Contains(first.Reason, "OpenBox could not record") {
				t.Errorf("the deny reason does not name the delivery failure: %q", first.Reason)
			}
			second, ok := run.DecisionFor(secondCall)
			if !ok || second.Verb != "deny" {
				t.Fatalf("second call of the SAME (now halted) run = %+v, want a deny too", second)
			}
			if n := countFiles(t, filepath.Join(run.Dir, "halts")); n != 1 {
				t.Errorf("%d session halt latch(es), want exactly 1", n)
			}
			grade(t, fakecore.HaltedAfterFailure(), sc, run.Run)
			grade(t, fakecore.OneAttempt(), sc, run.Run)
			if tc.verify != nil {
				tc.verify(t, run.Fake)
			}
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
// reach tool events. Enforcement is unconditional now (ResolveEnforce always
// reports true), so there is no "unenforced" scenario left to run these
// through the plain observe path; a genuinely ALLOWed gated call still never
// spools its own ToolCall/PromptSubmitted locally (the gate's escalation
// settles it, so SpoolObserve never fires -- see the second half below). The
// one place that DOES still queue the gated call's own DevEvent locally is
// when the escalation itself was never attempted at all (no client
// configured): EscalationNotAttempted falls back to SpoolObserve, same as
// the pre-enforcement observe path always did. That is what this drives
// directly, bypassing runScenario's own evalEnv (which always wires working
// credentials): an agent id (so the mapper's DID prefix check passes and the
// call is mappable) but no API key / workload key at all (so the client
// never builds and the escalation is never attempted).
func TestGovernanceEvalSpoolSeesToolEvents(t *testing.T) {
	spool := isolateHomeOnly(t)
	spool = filepath.Join(spool, "spool")
	t.Setenv(devconfig.EnvSpoolDir, spool)
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	t.Setenv(devconfig.EnvAgentID, fakecore.AgentID())
	// Deliberately absent: no OPENBOX_API_KEY / OPENBOX_WORKLOAD_PRIVATE_KEY,
	// so ResolveCredentials fails, the evaluator's client never builds, and
	// every gated call's escalation reports EscalationNotAttempted.

	for _, p := range []fakecore.HookPayload{sessionStart(), userPrompt("list the files"), preBash(okToolUseID, "ls -la"), postBash(okToolUseID, "ls -la")} {
		a, _, errb := testApp(nil)
		a.stdin = strings.NewReader(p.JSON)
		a.run([]string{"hook", "claude-code", p.Event})
		if panicked(errb.String()) {
			t.Fatalf("%s panicked: %s", p.Event, errb.String())
		}
	}

	observed := fakecore.SpooledEventTypes(spool)
	for _, want := range []string{"ToolCall", "ToolResult", "PromptSubmitted"} {
		if observed[want] == 0 {
			t.Errorf("no %s reached the spool with no reachable control plane, so ValidateDevEvent never saw one; spooled: %v", want, observed)
		}
	}
	validateSpool(t, spool, true, map[string]bool{})

	// And a genuinely gated + ALLOWed session cannot stand in for it: the
	// gate's own escalation settles the call, so its observe copy is
	// discarded. If this ever starts spooling them the scenario above is
	// redundant and should go -- but until then, dropping it would quietly
	// un-cover two types.
	trimmed := ranFineSession()
	trimmed.Payloads = trimmed.Payloads[:len(trimmed.Payloads)-1] // stop before the flush drains it
	enforced := fakecore.SpooledEventTypes(runScenario(t, trimmed).Spool)
	for _, gated := range []string{"ToolCall", "PromptSubmitted"} {
		if enforced[gated] != 0 {
			t.Errorf("an ALLOWed gated session spooled %s; the no-client scenario is no longer the only thing covering it: %v", gated, enforced)
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

// TestGovernanceEvalEnforceIsIgnoredButEveryGatedClassStillGates:
// enforcement is unconditional (devconfig.ResolveEnforce always reports
// true); `OPENBOX_ENFORCE=false` and dev.json's `enforce: false` still parse
// (so they can warn) but select nothing any more. The "warns once per
// process" half of this claim is devconfig's own tested invariant
// (posture_test.go, which resets the package-level sync.Once directly --
// unexported, and so unreachable from this package); what this test proves
// is the behavioral half only a governance eval can: a scenario that
// EXPLICITLY tries to turn enforcement off still runs the gate and still
// denies on a scripted deny, exactly as if it had never set the key at all.
// gatedClassFixture is one gated hook class, on one provider, expressed as
// the payload list that exercises it: SessionStart, the one gated call, then
// SessionEnd. Every class this table covers, both providers: Claude Code's
// PreToolUse/UserPromptSubmit/ConfigChange, Codex's own
// PreToolUse/UserPromptSubmit/PermissionRequest.
type gatedClassFixture struct {
	name     string
	provider string
	payloads []fakecore.HookPayload
}

func gatedClassFixtures() []gatedClassFixture {
	return []gatedClassFixture{
		{
			name:     "claude-code-PreToolUse",
			provider: "claude-code",
			payloads: []fakecore.HookPayload{sessionStart(), preBash("toolu_cc_pretooluse", "rm -rf /important"), sessionEnd()},
		},
		{
			name:     "claude-code-UserPromptSubmit",
			provider: "claude-code",
			payloads: []fakecore.HookPayload{sessionStart(), userPrompt("delete everything"), sessionEnd()},
		},
		{
			name:     "claude-code-ConfigChange",
			provider: "claude-code",
			payloads: []fakecore.HookPayload{
				sessionStart(),
				hook("ConfigChange", `{"hook_event_name":"ConfigChange","session_id":"`+evalSession+`","cwd":"/repo","source":"user_settings","file_path":"/home/dev/.claude/settings.json"}`),
				sessionEnd(),
			},
		},
		{
			name:     "codex-PreToolUse",
			provider: "codex",
			payloads: []fakecore.HookPayload{codexSessionStart(), codexPreBash("toolu_codex_pretooluse", "rm -rf /important"), codexSessionEnd()},
		},
		{
			name:     "codex-UserPromptSubmit",
			provider: "codex",
			payloads: []fakecore.HookPayload{codexSessionStart(), codexUserPrompt("delete everything"), codexSessionEnd()},
		},
		{
			name:     "codex-PermissionRequest",
			provider: "codex",
			payloads: []fakecore.HookPayload{
				codexSessionStart(),
				hook("PermissionRequest", `{"hook_event_name":"PermissionRequest","session_id":"`+evalSession+`","turn_id":"turn-1","cwd":"/repo","model":"gpt-5.3-codex","permission_mode":"default","tool_name":"Bash","tool_input":{"command":"rm -rf /important"}}`),
				codexSessionEnd(),
			},
		},
	}
}

// TestGovernanceEvalEnforceIsIgnoredButEveryGatedClassStillGates:
// `OPENBOX_ENFORCE=false` and dev.json's `enforce:false` are ignored for
// every gated class, on both providers -- a table over
// gatedClassFixtures() x {env var, dev.json key}. AlwaysStatus:500 forces an
// explicit, universal delivery failure regardless of the class or its own
// tool_use_id (Script.answer's per-id lookup never applies to a class with
// none, e.g. UserPromptSubmit/ConfigChange/PermissionRequest), so every
// class's own gated call denies -- whether from its OWN live escalation, or
// (when SessionStart's own inline attempt failed first) a replayed halt.
// Either way proves the SAME claim this test makes: enforce=false never lets
// a gated call through silently.
func TestGovernanceEvalEnforceIsIgnoredButEveryGatedClassStillGates(t *testing.T) {
	enforceSettings := []struct {
		name  string
		setup func(t fakecore.TB, fake *fakecore.Server)
	}{
		{"OPENBOX_ENFORCE=false", func(tb fakecore.TB, _ *fakecore.Server) {
			tb.(*testing.T).Setenv(devconfig.EnvEnforce, "false")
		}},
		{"dev.json enforce:false", func(tb fakecore.TB, _ *fakecore.Server) {
			t := tb.(*testing.T)
			cfgPath := filepath.Join(t.TempDir(), "dev-enforce-false.json")
			if err := os.WriteFile(cfgPath, []byte(`{"enforce":false}`), 0o600); err != nil {
				t.Fatalf("seed dev.json: %v", err)
			}
			// evalEnv itself never sets OPENBOX_CONFIG to a dev.json carrying
			// enforce:false; this overrides it, the one place in this suite
			// that does, and only to prove the key is ignored.
			t.Setenv(devconfig.EnvConfigPath, cfgPath)
		}},
	}

	for _, cls := range gatedClassFixtures() {
		for _, es := range enforceSettings {
			t.Run(cls.name+"/"+es.name, func(t *testing.T) {
				sc := fakecore.Scenario{
					Name:         "enforce-false-is-ignored-" + cls.name + "-still-gates",
					Provider:     cls.provider,
					Payloads:     cls.payloads,
					AlwaysStatus: 500,
					Setup:        es.setup,
					Provenance:   authoredProvenance,
				}
				run := runScenario(t, sc)

				var gated fakecore.Decision
				var found bool
				for _, d := range run.Decisions {
					if gatedEvents[d.Event] {
						gated, found = d, true
					}
				}
				// "block" is the vendor's own literal for the prompt/config
				// contracts (UserPromptSubmit, ConfigChange); "deny" is every
				// other gated class's own. Both mean the SAME thing: the gate
				// ran and refused, never a silent allow.
				if !found || (gated.Verb != "deny" && gated.Verb != "block") {
					t.Fatalf("%s / %s did not stop the gate from running: decision = %+v, want a deny/block", cls.name, es.name, gated)
				}
			})
		}
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
