package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
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
// captured. A recorded session is an upgrade phase 03 makes; labelling an
// authored one as recorded would be the lie that matters.
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

// TestGovernanceEvalPairing is the phase's real deliverable: proof the grader
// tells the two single-sided cases apart. The control that ran must pass, the
// control that was blocked must pass, and a dropped completion must fail.
func TestGovernanceEvalPairing(t *testing.T) {
	t.Run("a-tool-that-ran-is-reported-as-a-pair", func(t *testing.T) {
		sc := ranFineSession()
		run := runScenario(t, sc)
		inbox := run.Fake.Inbox()
		if len(inbox) == 0 {
			t.Fatal("nothing reached the fake; the flush did not deliver")
		}
		grade(t, fakecore.PairingGrader(), sc, inbox)
	})

	t.Run("a-blocked-tool-is-reported-once-not-twice", func(t *testing.T) {
		sc := blockedSession()
		run := runScenario(t, sc)
		// The deny must be real, not merely declared: the witness is only
		// honest if the binary actually refused the call.
		verb, reason := run.PermissionDecision(t, 2)
		if verb != "deny" {
			t.Fatalf("PreToolUse permissionDecision = %q (reason %q); the scenario's Denied witness claims this call was blocked, so the fixture is lying if it was not", verb, reason)
		}
		grade(t, fakecore.PairingGrader(), sc, run.Fake.Inbox())
	})

	t.Run("a-dropped-completion-is-named", func(t *testing.T) {
		sc := fakecore.Drop(ranFineSession(), "PostToolUse")
		run := runScenario(t, sc)
		reasons := fakecore.PairingGrader().Check(sc, run.Fake.Inbox())
		if len(reasons) != 1 {
			t.Fatalf("dropping the completion must produce exactly one named reason; got %d: %v", len(reasons), reasons)
		}
		if !strings.Contains(reasons[0], okToolUseID) {
			t.Errorf("the reason must name the call it is about; got %q", reasons[0])
		}
	})
}

// TestGovernanceEvalWitnessIsLoadBearing is the anti-vacuity control. Without
// the Denied witness a grader has only two options, and both are wrong: exempt
// every single-sided start, and a dropped completion passes; exempt none, and
// that is parity, which rejects the blocked session.
func TestGovernanceEvalWitnessIsLoadBearing(t *testing.T) {
	blocked := blockedSession()
	blockedInbox := runScenario(t, blocked).Fake.Inbox()

	if reasons := fakecore.PairingGrader().Check(blocked, blockedInbox); len(reasons) != 0 {
		t.Fatalf("the blocked session is healthy and must pass with its witness: %v", reasons)
	}
	stripped := fakecore.Undeny(blocked)
	if reasons := fakecore.PairingGrader().Check(stripped, blockedInbox); len(reasons) == 0 {
		t.Error("with the witness removed the grader accepted a single-sided start, which means it is exempting every single-sided start and a dropped completion would pass too")
	}
}

// TestGovernanceEvalMutations executes every grader's declared mutation and
// requires the named grader to go red. A grader nobody has watched fail is not
// yet a grader, and a drill recorded in a PR body is not a test.
func TestGovernanceEvalMutations(t *testing.T) {
	graders := []fakecore.Grader{fakecore.PairingGrader()}
	if len(graders) == 0 {
		t.Fatal("the grader registry is empty")
	}
	for _, g := range graders {
		t.Run(g.Name, func(t *testing.T) {
			if g.Mutate == nil {
				t.Fatalf("grader %q declares no mutation, so nothing proves it can fail", g.Name)
			}
			base := ranFineSession()
			mutated := g.Mutate(base)
			run := runScenario(t, mutated)
			reasons := g.Check(mutated, run.Fake.Inbox())
			if len(reasons) == 0 {
				t.Errorf("grader %q stayed green on its own declared mutation (%s); it cannot fail", g.Name, mutated.Name)
			}
			for _, r := range reasons {
				if strings.TrimSpace(r) == "" {
					t.Errorf("grader %q returned an empty reason; a reason nobody can read is a boolean", g.Name)
				}
			}
		})
	}
}

// TestGovernanceEvalRejectsAWrongKey: a body signed with a key core does not
// know is refused, and the event stays in the spool. A 401 must never spend a
// delivery attempt -- core answers 401 both for a bad key and for a fault of
// its own, so treating it as terminal would discard evidence over an outage.
func TestGovernanceEvalRejectsAWrongKey(t *testing.T) {
	sc := ranFineSession()
	fake := fakecore.New(t, sc.Script())

	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	evalEnv(t, fake, dir, spool)
	// A throwaway key the fake has never seen. Generated here rather than
	// committed: a fixture seed shared with the signer would let a broken
	// signer pass.
	t.Setenv(devconfig.EnvAgentPrivateKey, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("w", 32))))

	for _, p := range sc.Payloads {
		a, _, _ := testApp(nil)
		a.stdin = strings.NewReader(p.JSON)
		a.run([]string{"hook", "claude-code", p.Event})
	}

	if n := len(fake.Inbox()); n != 0 {
		t.Errorf("the fake accepted %d wrongly-signed request(s); it verifies nothing", n)
	}
	if fake.Hits() == 0 {
		t.Fatal("nothing reached the fake at all, so the rejection proves nothing")
	}
	if got := fake.Rejections(); len(got) == 0 || !strings.Contains(got[0], "signature") {
		t.Errorf("rejection reason should name the signature; got %v", got)
	}
	if !spoolStillHolds(t, spool) {
		t.Error("the spool was drained despite a 401; a 401 must not spend a delivery attempt, because core answers it for a database fault as well as a bad key")
	}
}

// TestGovernanceEvalRejectsASchemaInvalidEvent: the DevEvent validator reads
// the SPOOL, pre-flush. Not the wire body -- that is a different object with a
// four-value vocabulary, and pointing ValidateDevEvent at it would reject
// every event, after which the natural fix is to soft-fail the validator into
// a no-op.
func TestGovernanceEvalRejectsASchemaInvalidEvent(t *testing.T) {
	if err := conformance.ValidateDevEvent([]byte(`{"event_type":"tool_call"}`), false); err == nil {
		t.Fatal("a DevEvent missing every required field validated; the validator is a no-op")
	}
	// And the wire's own vocabulary is not a DevEvent: the two objects must
	// not be reachable by each other's validator.
	if err := conformance.ValidateDevEvent([]byte(`{"source":"developer-runtime","event_type":"ActivityStarted","workflow_id":"w","run_id":"r","timestamp":"2026-09-14T00:00:00Z"}`), false); err == nil {
		t.Fatal("a wire payload validated as a DevEvent; the two objects have been conflated")
	}
}

// evalEnv is runScenario's environment without the run, for the tests that
// need to drive the payloads themselves.
func evalEnv(t *testing.T, fake *fakecore.Server, dir, spool string) {
	t.Helper()
	t.Setenv(devconfig.EnvHome, filepath.Join(dir, "home"))
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv(devconfig.EnvConfigPath, filepath.Join(dir, "none.json"))
	t.Setenv(devconfig.EnvSpoolDir, spool)
	t.Setenv("OPENBOX_SESSION_DIR", filepath.Join(dir, "sessions"))
	t.Setenv(devconfig.EnvBaseURL, fake.URL())
	t.Setenv(devconfig.EnvDID, fake.DID())
	t.Setenv(devconfig.EnvAPIKeyDirect, "obx_test_"+strings.Repeat("a", 48))
	t.Setenv(devconfig.EnvAgentPrivateKey, fake.SeedB64())
	t.Setenv(devconfig.EnvEnforcementFile, filepath.Join(dir, "enforcements.jsonl"))
	t.Setenv(devconfig.EnvPendingApprovalDir, filepath.Join(dir, "pending-approvals"))
	t.Setenv(devconfig.EnvHaltDir, filepath.Join(dir, "halts"))
	t.Setenv("OPENBOX_ADVISORY_FILE", filepath.Join(dir, "advisories.jsonl"))
	t.Setenv(devconfig.EnvEnforce, "1")
	t.Setenv(devconfig.EnvFailClosed, "0")
	t.Setenv(devconfig.EnvContentCapture, "0")
	t.Setenv(devconfig.EnvRealtime, "0")
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
