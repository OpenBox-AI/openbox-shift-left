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
// the binary put on the wire and what it left on disk.
//
// The seam that removes the live `claude` dependency is stdin: a session IS an
// ordered list of native hook payloads, and the model's only job was
// generating that list. Frozen, it becomes an offline input generator.
//
// A grader takes (Scenario, inbox) and derives its expectation from the
// scenario's INPUT. An expectation read out of the same inbox it is checking
// is an identity, not a property.

// evalRun is everything one scenario produced: what reached the wire, what the
// coding agent read on stdout, and what was left on disk. The outcome family
// of graders needs the last of those -- a correct event stream describing a
// block that did not happen is still a failure.
type evalRun struct {
	Fake *fakecore.Server
	// Stdout and Stderr are index-aligned with Scenario.Payloads.
	Stdout []string
	Stderr []string
	// Dir holds the enforcement ledger, the halt latch, the pending-approval
	// markers and the spool for this scenario only.
	Dir   string
	Spool string
}

// EnforcementLedger reads the decisions actually applied, one JSON object per
// line. Empty when nothing was gated.
func (r evalRun) EnforcementLedger(t *testing.T) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.Dir, "enforcements.jsonl"))
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

// PermissionDecision reads the verb the coding agent actually received for the
// nth payload: "deny", "ask", "allow", or "" when the hook stayed silent.
// Parsed from the rendered stdout, never from the client's own Verdict -- the
// agent reads this, and nothing else.
func (r evalRun) PermissionDecision(t *testing.T, n int) (verb, reason string) {
	t.Helper()
	out := strings.TrimSpace(r.Stdout[n])
	if out == "" {
		return "", ""
	}
	var got struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("payload #%d stdout is not valid hook JSON: %v", n, err)
	}
	return got.HookSpecificOutput.PermissionDecision, got.HookSpecificOutput.PermissionDecisionReason
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
	fake := fakecore.New(t, sc.Script())

	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
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
	// Realtime delivery spawns the binary, and the trigger refuses a `*.test`
	// executable; TestHookRealtimeDelivery owns that path from a subprocess.
	// These evals prove the SessionEnd-flush path.
	t.Setenv(devconfig.EnvRealtime, "0")

	run := evalRun{Fake: fake, Dir: dir, Spool: spool}
	for i, p := range sc.Payloads {
		a, out, errb := testApp(nil)
		a.stdin = strings.NewReader(p.JSON)
		// The spooled DevEvents are validated against the contract before the
		// flush drains them. Not on the wire body: the wire is a different
		// object with a four-value vocabulary, and ValidateDevEvent pointed at
		// it would reject every event.
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
	}
	return run
}

// validateSpool reads the spooled DevEvents and holds each to the contract.
// This is the DevEvent half of the two-object split: Spool.Append writes
// client.DevEvent JSONL, so this is the only place the 33-type vocabulary is
// observable.
func validateSpool(t *testing.T, spoolDir string) {
	t.Helper()
	entries, err := os.ReadDir(spoolDir)
	if err != nil {
		return // nothing spooled yet
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(spoolDir, e.Name()))
		if err != nil {
			t.Fatalf("read spool %s: %v", e.Name(), err)
		}
		for i, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if err := conformance.ValidateDevEvent([]byte(line), false); err != nil {
				// The line itself is never echoed: INV-2 applies to test logs.
				t.Errorf("spooled event %s:%d is not contract-conformant: %v", e.Name(), i+1, err)
			}
		}
	}
}

// grade runs a grader and reports each reason. Reasons, not a boolean: a
// grader that can only say "false" cannot be acted on.
func grade(t *testing.T, g fakecore.Grader, sc fakecore.Scenario, inbox []fakecore.Received) {
	t.Helper()
	for _, r := range g.Check(sc, inbox) {
		t.Errorf("%s: %s", g.Name, r)
	}
}
