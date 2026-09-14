package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/conformance"
)

// Governance evals drive the real entrypoint -- `openbox hook claude-code
// <Event>`, payload on stdin -- against a fake control plane, then grade what
// the binary put on the wire, what it rendered to the coding agent, and what
// it left on disk.
//
// The seam that removes the live `claude` dependency is stdin: a session IS an
// ordered list of native hook payloads, and the model's only job was
// generating that list. Frozen, it becomes an offline input generator.
//
// A grader derives its expectation from the scenario's INPUT. An expectation
// read out of the same inbox it is checking is an identity, not a property.

// gatedEvents are the hook events that run the enforce gate, and therefore the
// ones that can render a decision. A payload outside this set produces no
// Decision entry at all, which is how a grader tells "not gated" from "gated
// and stayed silent".
var gatedEvents = map[string]bool{
	"PreToolUse":       true,
	"UserPromptSubmit": true,
}

type evalRun struct {
	fakecore.Run
	Fake *fakecore.Server
	// Stdout and Stderr are index-aligned with Scenario.Payloads.
	Stdout []string
	Stderr []string
	Spool  string
}

// runScenario drives one scenario end to end. Isolation is per scenario: a
// fresh spool, a fresh config path, and a pinned OPENBOX_HOME *and* HOME,
// because devconfig.Home falls back to $HOME/.openbox when OPENBOX_HOME is
// unset and a leaked read would reach the developer's real install.
//
// Deliberately no devconfig.Pin() here. RunHook already pins and releases per
// invocation, and Pin only clears the cache when its depth falls back to zero
// -- so an outer pin would hold the depth above zero and make every hook in
// the scenario reuse the first one's config. That is the opposite of the
// isolation it looks like.
func runScenario(t *testing.T, sc fakecore.Scenario) evalRun {
	t.Helper()
	requireUsableFixture(t, sc)

	fake := fakecore.New(t, sc.Script())
	if sc.Approval != nil {
		fake.Approval(sc.Approval)
	}
	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	evalEnv(t, fake, dir, spool, sc.Posture)

	run := evalRun{Fake: fake, Spool: spool}
	run.Dir = dir
	for i, p := range sc.Payloads {
		if sc.OutageDuring != nil {
			fake.SetOutage(sc.OutageDuring(i, p.Event))
		}
		a, out, errb := testApp(nil)
		a.stdin = strings.NewReader(p.JSON)
		// The spooled DevEvents are held to the contract before the flush
		// drains them. Not the wire body: that is a different object with a
		// four-value vocabulary, and ValidateDevEvent pointed at it would
		// reject every event.
		if p.Event == "SessionEnd" {
			validateSpool(t, spool)
		}
		if code := a.run([]string{"hook", "claude-code", p.Event}); code != exitOK {
			t.Fatalf("%s payload #%d exit = %d; stderr=%q", p.Event, i, code, errb.String())
		}
		// runHook always returns 0 and recovers panics, so the exit code says
		// nothing; stderr is where a panic surfaces.
		if strings.Contains(errb.String(), "recovered from panic") {
			t.Fatalf("%s payload #%d panicked: %s", p.Event, i, errb.String())
		}
		run.Stdout = append(run.Stdout, out.String())
		run.Stderr = append(run.Stderr, errb.String())
		if gatedEvents[p.Event] {
			run.Decisions = append(run.Decisions, decodeDecision(t, i, p, out.String()))
		}
	}

	run.Inbox = fake.Inbox()
	run.Ledger = readLedger(t, dir)
	// A refusal means the binary put something on the wire that core would
	// have rejected, or reached a route core does not serve. Either is a
	// finding, and leaving it for a grader to notice would let it pass as an
	// empty inbox. A failure the scenario scripted is counted separately and
	// is not one of these.
	if refused := fake.Rejections(); len(refused) > 0 {
		t.Errorf("the fake refused %d request(s): %s", len(refused), strings.Join(refused, " | "))
	}
	return run
}

// decodeDecision reads the verb the coding agent actually received. Parsed
// from the rendered stdout, never from the client's own Verdict -- the agent
// obeys these bytes and nothing else, and the two are not the same: a
// REQUIRE_APPROVAL that goes unanswered is rewritten to a HALT before it is
// ever rendered.
func decodeDecision(t *testing.T, i int, p fakecore.HookPayload, stdout string) fakecore.Decision {
	t.Helper()
	d := fakecore.Decision{Payload: i, Event: p.Event, ToolUseID: p.ToolUseID()}
	out := strings.TrimSpace(stdout)
	if out == "" {
		return d // gated, but the hook stayed silent: not the same as ungated
	}
	var got struct {
		Continue           *bool `json:"continue"`
		HookSpecificOutput struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("payload #%d (%s) stdout is not valid hook JSON: %v", i, p.Event, err)
	}
	d.Verb = got.HookSpecificOutput.PermissionDecision
	d.Reason = got.HookSpecificOutput.PermissionDecisionReason
	if d.Verb == "" && got.Decision != "" {
		// UserPromptSubmit renders `decision:"block"`; there is no permission
		// verb for a prompt.
		d.Verb = got.Decision
		d.Reason = got.Reason
	}
	d.Stop = got.Continue != nil && !*got.Continue
	return d
}

func readLedger(t *testing.T, dir string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "enforcements.jsonl"))
	if err != nil {
		return nil
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("enforcement ledger line is not JSON: %v", err)
		}
		out = append(out, m)
	}
	return out
}

// requireUsableFixture rejects a fixture that cannot produce the evidence a
// grader will look for. The mapper drops a payload with no session id
// fail-open, which would silently falsify any count derived from the payload
// list; and a tool payload with no tool_use_id would be graded through a
// fallback path production never takes.
func requireUsableFixture(t *testing.T, sc fakecore.Scenario) {
	t.Helper()
	for i, p := range sc.Payloads {
		if p.SessionID() == "" {
			t.Fatalf("payload #%d (%s) carries no session_id; the mapper drops it fail-open and the scenario would prove nothing", i, p.Event)
		}
		if strings.HasPrefix(p.Event, "PreToolUse") || strings.HasPrefix(p.Event, "PostToolUse") {
			if p.ToolUseID() == "" {
				t.Fatalf("payload #%d (%s) carries no tool_use_id; it would be graded through a fallback production does not take", i, p.Event)
			}
		}
	}
	if sc.Provenance == "" {
		t.Fatalf("scenario %q declares no provenance; a fixture whose origin is unrecorded cannot be audited", sc.Name)
	}
}

// validateSpool holds each spooled DevEvent to the contract. This is the
// DevEvent half of the two-object split.
//
// Note what it can and cannot see: under enforce, a gated PreToolUse egresses
// synchronously at the gate and its observe copy is discarded, so ToolCall and
// PromptSubmitted never reach the spool at all. The observe-only scenario is
// what puts those two under the validator.
func validateSpool(t *testing.T, spoolDir string) {
	t.Helper()
	for i, line := range fakecore.SpoolLines(spoolDir) {
		if err := conformance.ValidateDevEvent(line, false); err != nil {
			// The line itself is never echoed: INV-2 applies to test logs.
			t.Errorf("spooled event #%d is not contract-conformant: %v", i, err)
		}
	}
}

// grade runs a grader and reports each reason. Reasons, not a boolean: a
// grader that can only say "false" cannot be acted on.
func grade(t *testing.T, g fakecore.Grader, sc fakecore.Scenario, run fakecore.Run) {
	t.Helper()
	for _, r := range g.Check(sc, run) {
		t.Errorf("%s: %s", g.Name, r)
	}
}

func evalEnv(t *testing.T, fake *fakecore.Server, dir, spool string, p fakecore.Posture) {
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
	// Realtime delivery spawns the binary, and the trigger refuses a `*.test`
	// executable; TestHookRealtimeDelivery owns that path from a subprocess.
	// These evals prove the SessionEnd-flush path.
	t.Setenv(devconfig.EnvRealtime, "0")

	set := func(name, value, fallback string) {
		if value == "" {
			value = fallback
		}
		t.Setenv(name, value)
	}
	set(devconfig.EnvEnforce, p.Enforce, "1")
	set(devconfig.EnvFailClosed, p.FailClosed, "0")
	set(devconfig.EnvContentCapture, p.ContentCapture, "0")
	if p.ApprovalHoldMS != "" {
		t.Setenv(devconfig.EnvApprovalHold, p.ApprovalHoldMS)
	}
}

func stringsContainsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}
