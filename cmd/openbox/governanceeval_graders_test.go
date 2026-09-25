package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
)

// contentCanary is what a developer's own words look like in these scenarios.
// Deliberately not secret-shaped: this repository's own commit hook rewrites
// secret-shaped literals on write, so a canary that looked like a credential
// would be silently replaced in the source and the test would grade a
// placeholder.
const contentCanary = "CANARY-CONTENT-the-developers-own-words"

// awsSecret is assembled at run time for the same reason -- written out whole
// it would not survive being saved.
func awsSecret() string { return "AKIA" + strings.Repeat("R", 16) }

func preWrite(toolUseID, path, body string) fakecore.HookPayload {
	return hook("PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"`+evalSession+`","cwd":"/repo","tool_name":"Write","tool_use_id":"`+toolUseID+`","tool_input":{"file_path":"`+path+`","content":"`+body+`"}}`)
}

// capturedSession carries the canary through all three content fields: the
// prompt, a tool's input and a tool's output.
func capturedSession(capture string) fakecore.Scenario {
	return fakecore.Scenario{
		Name:    "content-" + capture,
		Posture: fakecore.Posture{ContentCapture: capture},
		Payloads: []fakecore.HookPayload{
			sessionStart(),
			userPrompt(contentCanary),
			preBash(okToolUseID, "echo "+contentCanary),
			postBash(okToolUseID, "echo "+contentCanary),
			sessionEnd(),
		},
		Provenance: authoredProvenance,
	}
}

// secretSession writes a credential-shaped value into a file body. The gate
// evaluates it, so the body is a candidate for attachment -- which is exactly
// where the redactor has to have already run.
func secretSession() fakecore.Scenario {
	return fakecore.Scenario{
		Name: "a-secret-reaches-neither-the-disk-nor-the-wire",
		// Content ON, so the body IS attached. With capture off the case
		// proves nothing: nothing egressed at all.
		Posture: fakecore.Posture{ContentCapture: "1"},
		Payloads: []fakecore.HookPayload{
			sessionStart(),
			preWrite(okToolUseID, "/repo/.env", awsSecret()),
			postBash(okToolUseID, "written"),
			sessionEnd(),
		},
		Provenance: authoredProvenance,
	}
}

// gradedScenario pairs a grader with the healthy scenario it must be green on
// and whose mutation must turn it red. The base belongs to the grader: a
// content grader has nothing to say about a session carrying no content, and
// registering it against an unrelated base would make its mutation a no-op.
type gradedScenario struct {
	grader fakecore.Grader
	base   func() fakecore.Scenario
	// whyNoMutation is required when the grader declares none. It exists so
	// "this grader has never been watched fail" can only be reached
	// deliberately and in writing -- and such a grader is proven red against a
	// synthetic run instead.
	whyNoMutation string
}

func gradedScenarios() []gradedScenario {
	return []gradedScenario{
		{grader: fakecore.PairingGrader(), base: ranFineSession},
		{grader: fakecore.CompletenessGrader(), base: ranFineSession},
		{grader: fakecore.ActivityTypeGrader(), base: ranFineSession},
		{grader: fakecore.DeliveryOnceGrader(), base: ranFineSession},
		{grader: fakecore.SignalArgsGrader(contentCanary), base: func() fakecore.Scenario { return capturedSession("1") }},
		{grader: fakecore.ContentGateGrader(contentCanary), base: func() fakecore.Scenario { return capturedSession("1") }},
		{grader: fakecore.RedactionGrader(awsSecret()), base: secretSession,
			whyNoMutation: "the gate redacts whenever secret detection OR content capture is on, so no scenario can switch redaction off; proven red against a synthetic run by TestGovernanceEvalRedactionGraderCanFail"},
		{grader: fakecore.StartFirst(), base: ranFineSession},
		{grader: fakecore.OneAttempt(), base: ranFineSession,
			whyNoMutation: "no sequence of distinct native hook payloads can produce a same-key double-send: claude-code's own mapper derives EventID from a fresh, per-invocation high-resolution timestamp (INV-5), so two separate hook processes for identical content still mint two different keys. Proven red directly, against a hand-built Run, by internal/client/fakecore/graderreasons_test.go's own table"},
		{grader: fakecore.HaltedAfterFailure(), base: ranFineSession,
			whyNoMutation: "no input mutation can un-halt a healthy binary for one call and not the next; the only thing that could make this grader fail is a code regression in the latch read on a later gated call, which no scenario fixture expresses. Proven red directly, against hand-built Runs, by internal/client/fakecore/graderreasons_test.go's own table"},
	}
}

// TestGovernanceEvalGraders runs every grader against its own healthy control.
func TestGovernanceEvalGraders(t *testing.T) {
	for _, gs := range gradedScenarios() {
		t.Run(gs.grader.Name, func(t *testing.T) {
			sc := gs.base()
			grade(t, gs.grader, sc, runScenario(t, sc).Run)
		})
	}
}

// TestGovernanceEvalContentGateBothDirections: capture off hides the
// developer's words, capture on delivers them, and the canary must land in a
// content field rather than anywhere in the body.
//
// Only the off direction is the obvious one to write, and a suite with only
// that direction produces a system that over-captures: capture could be broken
// outright and every test would still pass.
func TestGovernanceEvalContentGateBothDirections(t *testing.T) {
	for _, capture := range []string{"0", "1"} {
		t.Run("capture-"+capture, func(t *testing.T) {
			sc := capturedSession(capture)
			run := runScenario(t, sc)
			grade(t, fakecore.ContentGateGrader(contentCanary), sc, run.Run)
			grade(t, fakecore.SignalArgsGrader(contentCanary), sc, run.Run)
		})
	}
}

// TestGovernanceEvalRedactionRunsBeforeAttachment is the security-critical
// one. Redaction running after the body is attached would leave the wire copy
// holding the secret while the local file looked clean, so both halves are
// asserted -- and the mutation turns detection off, which does not move the
// expectation with it: the secret must never appear either way.
func TestGovernanceEvalRedactionRunsBeforeAttachment(t *testing.T) {
	sc := secretSession()
	run := runScenario(t, sc)
	grade(t, fakecore.RedactionGrader(awsSecret()), sc, run.Run)

	// The case proves nothing if no content egressed at all: with capture on,
	// the body must have been attached and rewritten, not dropped.
	var sawRedaction bool
	for _, r := range run.Inbox {
		if strings.Contains(string(r.Raw), "OPENBOX_REDACTED") {
			sawRedaction = true
		}
	}
	if !sawRedaction {
		t.Error("no delivered body carries a redaction marker; the secret may simply never have been attached, in which case the ordering was never exercised")
	}
}

// TestGovernanceEvalRedactionGraderCanFail watches the one grader no scenario
// can falsify actually fail.
//
// It is proven against a hand-built run rather than a mutated scenario because
// the behaviour it checks cannot be switched off from a scenario -- the gate
// redacts whenever secret detection or content capture is on. Writing a
// mutation that could not bite and calling it executed would be worse than
// saying so.
func TestGovernanceEvalRedactionGraderCanFail(t *testing.T) {
	secret := awsSecret()
	g := fakecore.RedactionGrader(secret)

	leaked := fakecore.Run{Inbox: []fakecore.Received{{
		Raw:  []byte(`{"event_type":"ActivityStarted","activity_input":{"content":"` + secret + `"}}`),
		Body: map[string]any{"event_type": "ActivityStarted"},
	}}}
	if reasons := g.Check(fakecore.Scenario{}, leaked); len(reasons) == 0 {
		t.Error("the redaction grader accepted a body carrying the secret verbatim")
	}

	onDisk := t.TempDir()
	if err := os.WriteFile(filepath.Join(onDisk, "spooled.jsonl"), []byte(secret), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	reasons := g.Check(fakecore.Scenario{}, fakecore.Run{Dir: onDisk})
	if len(reasons) == 0 {
		t.Error("the redaction grader accepted a secret persisted to disk; only the wire half is being checked")
	}
	for _, r := range reasons {
		if strings.Contains(r, secret) {
			t.Error("the grader quoted the secret in its own reason; a test log is an egress too")
		}
	}
}
