package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// testEvalKey is an obx_ runtime key shape the client accepts (non-empty); the
// fake /evaluate server never verifies it.
const testEvalKey = "obx_test_tier2000000000000000000000000000000"

func TestIsHighRiskClass(t *testing.T) {
	cases := map[string]bool{
		"Bash":                      true,  // arbitrary shell execution
		"mcp__github__create_issue": true,  // MCP tool call
		"mcp__db__query":            true,  // MCP tool call
		"Edit":                      false, // file — T1 only
		"Write":                     false, // file — T1 only
		"Read":                      false, // file — T1 only
		"WebFetch":                  false, // shell-catch-all, not arbitrary exec
		"Task":                      false, // shell-catch-all
		"TodoWrite":                 false, // shell-catch-all
	}
	for tool, want := range cases {
		if got := isHighRiskClass(tool); got != want {
			t.Errorf("isHighRiskClass(%q) = %v, want %v", tool, got, want)
		}
	}
}

func TestDecisionTightens(t *testing.T) {
	tighten := []client.Verdict{client.VerdictHalt, client.VerdictBlock, client.VerdictRequireApproval}
	for _, v := range tighten {
		if !decisionTightens(decision.Decision{Evaluation: client.Evaluation{Verdict: v}}) {
			t.Errorf("decisionTightens(%s) = false, want true", v)
		}
	}
	proceed := []client.Verdict{client.VerdictAllow, client.VerdictConstrain, client.VerdictUnknown}
	for _, v := range proceed {
		if decisionTightens(decision.Decision{Evaluation: client.Evaluation{Verdict: v}}) {
			t.Errorf("decisionTightens(%s) = true, want false", v)
		}
	}
	// A failed guardrail tightens (deny) regardless of a non-block verdict.
	gf := decision.Decision{Evaluation: client.Evaluation{
		Verdict:   client.VerdictAllow,
		Guardrail: &client.GuardrailResult{Passed: false, Reasons: []client.GuardrailReason{{Type: "pii"}}},
	}}
	if !decisionTightens(gf) {
		t.Error("decisionTightens(allow + failed guardrail) = false, want true")
	}
}

func TestEvaluationDecision(t *testing.T) {
	// A real verdict is carried through, hookflow.FailOpen=false, so fail-closed never
	// overrides a reachable-core allow.
	real := hookflow.EvaluationDecision(client.Evaluation{Verdict: client.VerdictBlock, Reason: "policy"})
	if real.FailOpen {
		t.Error("a real BLOCK verdict must be hookflow.FailOpen=false")
	}
	if real.Source != hookflow.SourceEvaluate {
		t.Errorf("Source = %q, want %q", real.Source, hookflow.SourceEvaluate)
	}
	if real.Evaluation.Verdict != client.VerdictBlock {
		t.Errorf("verdict = %q, want BLOCK", real.Evaluation.Verdict)
	}
	allow := hookflow.EvaluationDecision(client.Evaluation{Verdict: client.VerdictAllow})
	if allow.FailOpen {
		t.Error("a real ALLOW verdict must be hookflow.FailOpen=false")
	}
	// No verdict (Emit fail-open drop / empty response) → fail-open decision.
	unknown := hookflow.EvaluationDecision(client.Evaluation{Verdict: client.VerdictUnknown})
	if !unknown.FailOpen {
		t.Error("VerdictUnknown must be hookflow.FailOpen=true (no real server verdict)")
	}
	if unknown.Source != hookflow.SourceEvaluateFailOpen {
		t.Errorf("Source = %q, want %q", unknown.Source, hookflow.SourceEvaluateFailOpen)
	}
}

func TestEvaluationFailOpen(t *testing.T) {
	d := hookflow.EvaluationFailOpen("evaluation credentials unavailable")
	if !d.FailOpen || d.Source != hookflow.SourceEvaluateFailOpen {
		t.Errorf("tier2FailOpen must be hookflow.FailOpen + fail-open source, got %+v", d)
	}
	if d.Evaluation.Verdict != client.VerdictUnknown {
		t.Errorf("verdict = %q, want UNKNOWN", d.Evaluation.Verdict)
	}
	// The cause becomes the fail-closed reason via failClosedReason — content-free.
	if d.Evaluation.Reason != "evaluation credentials unavailable" {
		t.Errorf("reason = %q, want the content-free cause", d.Evaluation.Reason)
	}
}

func TestResolveTier2(t *testing.T) {
	isolateConfig(t) // no config → default OFF
	if ResolveTier2() {
		t.Error("ResolveTier2 default must be OFF (opt-in)")
	}
	t.Setenv(envTier2, "1")
	if !ResolveTier2() {
		t.Error("OPENBOX_TIER2=1 must enable T2")
	}
	t.Setenv(envTier2, "0")
	if ResolveTier2() {
		t.Error("OPENBOX_TIER2=0 must disable T2")
	}
}

func TestResolveEvaluationTimeout(t *testing.T) {
	isolateConfig(t)
	if got := ResolveEvaluationTimeout(); got != hookflow.DefaultEvaluationTimeout {
		t.Errorf("default = %v, want %v", got, hookflow.DefaultEvaluationTimeout)
	}
	t.Setenv(envTier2Timeout, "1000")
	if got := ResolveEvaluationTimeout(); got != time.Second {
		t.Errorf("1000ms env = %v, want 1s", got)
	}
	// Over the clamp → maxEvaluationTimeout (correctness bound under the 5s hook timeout).
	t.Setenv(envTier2Timeout, "60000")
	if got := ResolveEvaluationTimeout(); got != maxEvaluationTimeout {
		t.Errorf("60000ms env = %v, want the %v clamp", got, maxEvaluationTimeout)
	}
	// Garbage env is ignored → default (never wipes a valid config silently).
	t.Setenv(envTier2Timeout, "notanumber")
	if got := ResolveEvaluationTimeout(); got != hookflow.DefaultEvaluationTimeout {
		t.Errorf("garbage env = %v, want default %v", got, hookflow.DefaultEvaluationTimeout)
	}
	// A margin invariant: the max T2 budget must stay under the shipped hook timeout.
	if maxEvaluationTimeout >= 5*time.Second {
		t.Errorf("maxEvaluationTimeout %v must stay under CC's 5s hook timeout (fails OPEN)", maxEvaluationTimeout)
	}
}

// TestPinnedClockStableEventID proves the MINOR-1 fix: with the Mapper clock pinned
// (as RunHook does per-invocation), mapping the SAME PreToolUse payload twice — the
// Observe spool copy + the T2 /evaluate copy — yields the SAME deterministic
// event_id, so the two collapse under one Idempotency-Key (OD-SYNC-11). With the
// default time.Now clock they would differ by the sub-second timestamp.
func TestPinnedClockStableEventID(t *testing.T) {
	ev := &HookEvent{HookEventName: "PreToolUse", SessionID: "s", Cwd: "/tmp", ToolName: "Bash",
		ToolInput: []byte(`{"command":"echo hi"}`)}

	pinned := time.Now()
	m := NewMapper(Identity{DeveloperDID: testDID})
	m.Now = func() time.Time { return pinned }
	e1, ok1 := m.Map(HookPreToolUse, ev)
	e2, ok2 := m.Map(HookPreToolUse, ev)
	if !ok1 || !ok2 {
		t.Fatal("Map should succeed for a valid PreToolUse event")
	}
	if e1.EventID == "" || e1.EventID != e2.EventID {
		t.Errorf("pinned clock must yield a stable event_id: %q vs %q", e1.EventID, e2.EventID)
	}

	// Sanity: an UNpinned clock would (almost always) differ — proving the pin is
	// load-bearing. Two Nano-precision instants a tick apart derive distinct ids.
	mu := NewMapper(Identity{DeveloperDID: testDID})
	u1, _ := mu.Map(HookPreToolUse, ev)
	time.Sleep(2 * time.Millisecond)
	u2, _ := mu.Map(HookPreToolUse, ev)
	if u1.EventID == u2.EventID {
		t.Errorf("unpinned clock unexpectedly produced identical ids (%q) — the pin fix would be moot", u1.EventID)
	}
}

func TestTier2Budget(t *testing.T) {
	isolateConfig(t) // default T2 budget = hookflow.DefaultEvaluationTimeout (3.5s)
	// Just started: remaining (~4s) exceeds the default, so the budget is the default.
	if got := evaluationBudget(time.Now()); got != hookflow.DefaultEvaluationTimeout {
		t.Errorf("fresh enforceStart budget = %v, want default %v", got, hookflow.DefaultEvaluationTimeout)
	}
	// T1 already consumed the whole-hook cap → the remainder (and thus the budget) is
	// non-positive, so escalateEvaluation fail-opens immediately rather than overrun the hook.
	if got := evaluationBudget(time.Now().Add(-hookflow.EnforceBudget(Engine{}.HookCeilings()) - time.Second)); got > 0 {
		t.Errorf("exhausted-cap budget = %v, want <= 0 (immediate fail-open)", got)
	}
}

// The verdict-before-ceiling pin. Since ADR-0017 every gated call waits on a
// network verdict, so this is a security control rather than arithmetic
// hygiene: a hook killed before it writes lets the tool run, and no org setting
// — not even fail_closed — can stop it, because there is no hook left to deny
// with.
//
// It is stated against the SPI-declared ceiling and the timeout the installer
// actually writes, never a literal, so raising one raises the other and the two
// cannot drift apart silently.
func TestEnforceBudgetStaysUnderTheDeclaredCeiling(t *testing.T) {
	ceiling := Engine{}.HookCeilings()
	installed := time.Duration(preToolUseHookTimeoutSec) * time.Second
	if ceiling.Gating != installed {
		t.Errorf("declared gating ceiling %v must equal the installed PreToolUse timeout %v",
			ceiling.Gating, installed)
	}
	budget := hookflow.EnforceBudget(ceiling)
	if budget >= ceiling.Gating {
		t.Errorf("whole-hook budget %v must stay strictly under the ceiling %v; "+
			"a hook killed mid-gate lets the tool run ungoverned", budget, ceiling.Gating)
	}
	if maxEvaluationTimeout > budget {
		t.Errorf("the evaluation clamp %v must stay within the whole-hook budget %v",
			maxEvaluationTimeout, budget)
	}
	if ceiling.Other <= 0 {
		t.Error("a non-gating ceiling of zero would make every non-gate hook budget negative")
	}
}

// TestInstalledHookTimeoutMatchesThePlugin pins the plugin's PreToolUse
// `timeout` to the constant the enforce budgets derive from. They are in two
// files — a JSON bundle and Go — so nothing but this test stops them drifting,
// and a drift means the engine budgets against a ceiling the provider is not
// enforcing.
func TestInstalledHookTimeoutMatchesThePlugin(t *testing.T) {
	raw, err := pluginFS.ReadFile("plugin/hooks/hooks.json")
	if err != nil {
		t.Fatalf("read embedded hooks.json: %v", err)
	}
	var f struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command       string `json:"command"`
				Timeout       int    `json:"timeout"`
				StatusMessage string `json:"statusMessage"`
				AsyncRewake   bool   `json:"asyncRewake"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse hooks.json: %v", err)
	}
	groups := f.Hooks["PreToolUse"]
	if len(groups) != 1 || len(groups[0].Hooks) != 2 {
		t.Fatalf("PreToolUse should register the gate and the rewake watcher, got %+v", groups)
	}
	gate, watcher := groups[0].Hooks[0], groups[0].Hooks[1]

	if gate.AsyncRewake {
		t.Fatal("the GATE must be synchronous — an asyncRewake handler cannot block a tool call")
	}
	if gate.Timeout != preToolUseHookTimeoutSec {
		t.Errorf("hooks.json PreToolUse timeout = %d, want preToolUseHookTimeoutSec = %d", gate.Timeout, preToolUseHookTimeoutSec)
	}
	// Without a status message a hold reads as a freeze rather than a reason.
	if gate.StatusMessage == "" {
		t.Error("the gating hook needs a statusMessage so a hold shows a reason")
	}

	if !watcher.AsyncRewake {
		t.Error("the watcher must set asyncRewake — that is the only channel that wakes a session on exit 2")
	}
	if !strings.Contains(watcher.Command, "rewake claude-code") {
		t.Errorf("watcher command = %q, want the rewake subcommand", watcher.Command)
	}
	// The watcher has to outlive core's approval window or a late decision
	// lands unannounced.
	if watcher.Timeout != rewakeHookTimeoutSec || time.Duration(watcher.Timeout)*time.Second < 30*time.Minute {
		t.Errorf("watcher timeout = %ds, want rewakeHookTimeoutSec (%ds) covering the approval window",
			watcher.Timeout, rewakeHookTimeoutSec)
	}

	// Only the gating hook carries the raised ceiling; the rest stay tight.
	for event, gs := range f.Hooks {
		if event == "PreToolUse" || event == "SessionEnd" {
			continue
		}
		for _, g := range gs {
			for _, hh := range g.Hooks {
				if hh.Timeout >= preToolUseHookTimeoutSec {
					t.Errorf("%s timeout = %d — only the gating hook may carry the raised ceiling", event, hh.Timeout)
				}
			}
		}
	}
}

// serveEvaluate stands up a fake /evaluate endpoint returning verdictJSON (the
// core wire shape) and counts POSTs. On 127.0.0.1 → http loopback is accepted by
// the client's checkBaseURL. delay simulates a slow core.
func serveEvaluate(t *testing.T, verdictJSON string, status int, delay time.Duration) (url string, hits *int32) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(verdictJSON))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

// evalCreds points the hot-path credential resolver at a fake core with a valid
// signing seed (no secret store).
// serveVerdict stands up the control plane for a case and points the adapter at
// it. It replaces setBundleEnv: since ADR-0017 a case's expected outcome is a
// SERVER verdict, not a local bundle, so the setup names the verdict directly.
func serveVerdict(t *testing.T, verdictJSON string) {
	t.Helper()
	url, _ := serveEvaluate(t, verdictJSON, 200, 0)
	evalCreds(t, url)
}

func evalCreds(t *testing.T, baseURL string) {
	t.Helper()
	t.Setenv(envBaseURL, baseURL)
	t.Setenv(envAPIKeyDirect, testEvalKey)
	t.Setenv(envAgentPrivateKey, testPrivateKeyB64)
	t.Setenv(envContentCapture, "0")
}

func TestEscalateTier2_RealVerdict(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	url, hits := serveEvaluate(t, `{"verdict":"block","reason":"tier2 exec policy","policy_id":"t2-pol"}`, 200, 0)
	evalCreds(t, url)

	m := NewMapper(Identity{DeveloperDID: testDID})
	ev := &HookEvent{HookEventName: "PreToolUse", SessionID: "s", Cwd: "/tmp", ToolName: "Bash",
		ToolInput: []byte(`{"command":"rm -rf /tmp/x"}`)}
	dec := escalateEvaluation(context.Background(), log.New(&nopWriter{}, "", 0), m, ev, time.Second)

	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("/evaluate hits = %d, want 1", atomic.LoadInt32(hits))
	}
	if dec.FailOpen {
		t.Error("a real BLOCK from core must be hookflow.FailOpen=false")
	}
	if dec.Evaluation.Verdict != client.VerdictBlock {
		t.Errorf("verdict = %q, want BLOCK", dec.Evaluation.Verdict)
	}
	if dec.Source != hookflow.SourceEvaluate {
		t.Errorf("Source = %q, want %q", dec.Source, hookflow.SourceEvaluate)
	}
}

func TestEscalateTier2_Outage(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	// A 500 exhausts the client's retries and Emit fails open → no verdict.
	url, _ := serveEvaluate(t, `boom`, 500, 0)
	evalCreds(t, url)

	m := NewMapper(Identity{DeveloperDID: testDID})
	ev := &HookEvent{HookEventName: "PreToolUse", SessionID: "s", Cwd: "/tmp", ToolName: "Bash",
		ToolInput: []byte(`{"command":"echo hi"}`)}
	dec := escalateEvaluation(context.Background(), log.New(&nopWriter{}, "", 0), m, ev, time.Second)
	if !dec.FailOpen {
		t.Error("a /evaluate outage must yield a fail-open decision")
	}
	if dec.Source != hookflow.SourceEvaluateFailOpen {
		t.Errorf("Source = %q, want %q", dec.Source, hookflow.SourceEvaluateFailOpen)
	}
}

func TestEscalateTier2_NoCredentials(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	// No API key / seed configured → ResolveCredentials fails → degrade fail-open.
	url, hits := serveEvaluate(t, `{"verdict":"allow"}`, 200, 0)
	t.Setenv(envBaseURL, url)
	m := NewMapper(Identity{DeveloperDID: testDID})
	ev := &HookEvent{HookEventName: "PreToolUse", SessionID: "s", Cwd: "/tmp", ToolName: "Bash",
		ToolInput: []byte(`{"command":"echo hi"}`)}
	dec := escalateEvaluation(context.Background(), log.New(&nopWriter{}, "", 0), m, ev, time.Second)
	if !dec.FailOpen {
		t.Error("missing credentials must degrade to a fail-open decision")
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Errorf("no /evaluate call should be made without credentials; hits=%d", atomic.LoadInt32(hits))
	}
}

func TestEscalateTier2_BudgetBound(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	// A server slower than the budget: escalateEvaluation must return within ~budget
	// (the ctx cancels Emit), not wait for the server.
	url, _ := serveEvaluate(t, `{"verdict":"allow"}`, 200, 2*time.Second)
	evalCreds(t, url)
	m := NewMapper(Identity{DeveloperDID: testDID})
	ev := &HookEvent{HookEventName: "PreToolUse", SessionID: "s", Cwd: "/tmp", ToolName: "Bash",
		ToolInput: []byte(`{"command":"echo hi"}`)}
	start := time.Now()
	dec := escalateEvaluation(context.Background(), log.New(&nopWriter{}, "", 0), m, ev, 150*time.Millisecond)
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Errorf("escalateEvaluation took %v, must return near the 150ms budget", elapsed)
	}
	if !dec.FailOpen {
		t.Error("a budget-exceeding /evaluate must yield a fail-open decision")
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// ── STORY-E6-S10 conformance: Tier-2 sync /evaluate escalation (C12-C17) ──────
//
// These drive the REAL RunHook PreToolUse path end-to-end against a REAL local
// sidecar (T1) + a fake /evaluate core (T2), asserting the exact CC stdout and
// that /evaluate is called ONLY for high-risk classes when T1 allows and T2 is on.
func TestEnforcementConformance_Tier2(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envEnforcementFile, filepath.Join(t.TempDir(), "enf.jsonl"))
	t.Setenv(envContentCapture, "0")
	t.Setenv(envEnforce, "1")

	const dangerBash = `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"rm -rf /tmp/x"}}`
	const benignBash = `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`
	const editCall = `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Edit","tool_input":{"file_path":"/tmp/a.txt","old_string":"x","new_string":"y"}}`

	run := func(payload string) string {
		var stdout bytes.Buffer
		RunHook("PreToolUse", strings.NewReader(payload), &stdout, log.New(&nopWriter{}, "", 0))
		return stdout.String()
	}
	// allowT1 serves a reachable, bundled sidecar whose default is allow, so T1 always
	// proceeds and the escalation decision is entirely T2's.
	allowT1 := func(t *testing.T) {
		serveVerdict(t, `{"verdict":"allow"}`)
	}

	t.Run("C12 T1 allow + T2 BLOCK on Bash → deny (floor closed)", func(t *testing.T) {
		allowT1(t)
		url, hits := serveEvaluate(t, `{"verdict":"block","reason":"tier2 exec policy","policy_id":"t2-pol"}`, 200, 0)
		evalCreds(t, url)
		t.Setenv(envTier2, "1")
		t.Setenv(envFailClosed, "0")
		out := run(dangerBash)
		d, reason := parsePermissionDecision(t, []byte(out))
		if d != ccDecisionDeny {
			t.Fatalf("permissionDecision = %q, want deny (T2 must close the T1-allow floor); stdout=%q", d, out)
		}
		if !strings.Contains(reason, "tier2 exec policy") || !strings.Contains(reason, "t2-pol") {
			t.Errorf("reason = %q, want the T2 policy reason + id", reason)
		}
		if atomic.LoadInt32(hits) != 1 {
			t.Errorf("/evaluate hits = %d, want exactly 1", atomic.LoadInt32(hits))
		}
		if strings.Contains(out, "rm -rf") {
			t.Errorf("stdout leaked the command (INV-2): %q", out)
		}
	})

	t.Run("C13 T1 allow + T2 ALLOW on Bash → proceed", func(t *testing.T) {
		allowT1(t)
		url, hits := serveEvaluate(t, `{"verdict":"allow"}`, 200, 0)
		evalCreds(t, url)
		t.Setenv(envTier2, "1")
		t.Setenv(envFailClosed, "0")
		if out := run(benignBash); strings.TrimSpace(out) != "" {
			t.Errorf("a real T2 ALLOW must proceed (empty stdout); got %q", out)
		}
		if atomic.LoadInt32(hits) != 1 {
			t.Errorf("/evaluate hits = %d, want exactly 1", atomic.LoadInt32(hits))
		}
	})

	t.Run("C14 every gated class evaluates inline, incl. Edit (ADR-0017)", func(t *testing.T) {
		// The inverse of what this asserted before. Edit used to be decided
		// locally and never reached the server, which is precisely how a
		// raw-rego org — whose policy cannot be evaluated locally at all — was
		// left ungoverned on everything but shell and MCP.
		allowT1(t)
		url, hits := serveEvaluate(t, `{"verdict":"block","reason":"edit policy","policy_id":"e-pol"}`, 200, 0)
		evalCreds(t, url)
		t.Setenv(envFailClosed, "0")
		out := run(editCall)
		d, reason := parsePermissionDecision(t, []byte(out))
		if d != ccDecisionDeny {
			t.Fatalf("Edit must be decided by /evaluate; permissionDecision = %q, want deny (stdout=%q)", d, out)
		}
		if !strings.Contains(reason, "edit policy") {
			t.Errorf("reason = %q, want the server's policy reason", reason)
		}
		if atomic.LoadInt32(hits) != 1 {
			t.Errorf("/evaluate hits = %d, want exactly 1 for Edit", atomic.LoadInt32(hits))
		}
	})

	t.Run("C15 T2 outage: fail-open proceeds / fail-closed denies within bound", func(t *testing.T) {
		allowT1(t)
		url, _ := serveEvaluate(t, `boom`, 500, 0) // exhausts retries → Emit fails open
		evalCreds(t, url)
		t.Setenv(envTier2, "1")

		t.Setenv(envFailClosed, "0")
		if out := run(dangerBash); strings.TrimSpace(out) != "" {
			t.Errorf("T2 outage under fail-open must proceed; got %q", out)
		}

		t.Setenv(envFailClosed, "1")
		start := time.Now()
		out := run(dangerBash)
		elapsed := time.Since(start)
		d, reason := parsePermissionDecision(t, []byte(out))
		if d != ccDecisionDeny {
			t.Fatalf("T2 outage under fail-closed must deny; got %q (stdout=%q)", d, out)
		}
		if !strings.Contains(reason, "fail-closed") {
			t.Errorf("reason = %q, want the content-free fail-closed reason", reason)
		}
		// The in-binary budget must emit the deny well before CC's 5s hook timeout
		// (which fails OPEN) — OD-SYNC-8.
		if elapsed > 4500*time.Millisecond {
			t.Errorf("fail-closed deny took %v; must land before CC's 5s hook timeout", elapsed)
		}
		if strings.Contains(out, "rm -rf") {
			t.Errorf("stdout leaked the command (INV-2): %q", out)
		}
	})

	t.Run("C16 the deprecated tier2=0 opt-out no longer disables evaluation", func(t *testing.T) {
		// Back-compat with teeth: the key still parses, but an org that set it
		// under the old design must not silently keep its gates open. Honouring
		// it would leave exactly the population most likely to have set it —
		// early adopters of enforcement — ungoverned after upgrading.
		allowT1(t)
		url, hits := serveEvaluate(t, `{"verdict":"block","reason":"still governed"}`, 200, 0)
		evalCreds(t, url)
		t.Setenv(envTier2, "0") // deprecated: parsed, ignored
		t.Setenv(envFailClosed, "0")
		out := run(dangerBash)
		if d, _ := parsePermissionDecision(t, []byte(out)); d != ccDecisionDeny {
			t.Fatalf("tier2=0 must not suppress the verdict; permissionDecision = %q, want deny (stdout=%q)", d, out)
		}
		if atomic.LoadInt32(hits) != 1 {
			t.Errorf("/evaluate hits = %d, want 1 — the opt-out is ignored", atomic.LoadInt32(hits))
		}
	})

	// C17 (a local BLOCK short-circuits the round-trip) is deleted with the local
	// evaluator. There is no local verdict to short-circuit on, and the
	// round-trip is no longer an optimisation to skip — it is where the verdict
	// comes from. The property it protected, that governance only ever tightens,
	// now lives entirely in the server's answer and the apply cascade.
}
