package claudecode

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/sidecar"
)

// testTier2Key is an obx_ runtime key shape the client accepts (non-empty); the
// fake /evaluate server never verifies it.
const testTier2Key = "obx_test_tier2000000000000000000000000000000"

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
		if !decisionTightens(sidecar.Decision{Evaluation: client.Evaluation{Verdict: v}}) {
			t.Errorf("decisionTightens(%s) = false, want true", v)
		}
	}
	proceed := []client.Verdict{client.VerdictAllow, client.VerdictConstrain, client.VerdictUnknown}
	for _, v := range proceed {
		if decisionTightens(sidecar.Decision{Evaluation: client.Evaluation{Verdict: v}}) {
			t.Errorf("decisionTightens(%s) = true, want false", v)
		}
	}
	// A failed guardrail tightens (deny) regardless of a non-block verdict.
	gf := sidecar.Decision{Evaluation: client.Evaluation{
		Verdict:   client.VerdictAllow,
		Guardrail: &client.GuardrailResult{Passed: false, Reasons: []client.GuardrailReason{{Type: "pii"}}},
	}}
	if !decisionTightens(gf) {
		t.Error("decisionTightens(allow + failed guardrail) = false, want true")
	}
}

func TestTier2Decision(t *testing.T) {
	// A real verdict is carried through, FailOpen=false, so fail-closed never
	// overrides a reachable-core allow.
	real := tier2Decision(client.Evaluation{Verdict: client.VerdictBlock, Reason: "policy"})
	if real.FailOpen {
		t.Error("a real BLOCK verdict must be FailOpen=false")
	}
	if real.Source != sourceTier2 {
		t.Errorf("Source = %q, want %q", real.Source, sourceTier2)
	}
	if real.Evaluation.Verdict != client.VerdictBlock {
		t.Errorf("verdict = %q, want BLOCK", real.Evaluation.Verdict)
	}
	allow := tier2Decision(client.Evaluation{Verdict: client.VerdictAllow})
	if allow.FailOpen {
		t.Error("a real ALLOW verdict must be FailOpen=false")
	}
	// No verdict (Emit fail-open drop / empty response) → fail-open decision.
	unknown := tier2Decision(client.Evaluation{Verdict: client.VerdictUnknown})
	if !unknown.FailOpen {
		t.Error("VerdictUnknown must be FailOpen=true (no real server verdict)")
	}
	if unknown.Source != sourceTier2FailOpen {
		t.Errorf("Source = %q, want %q", unknown.Source, sourceTier2FailOpen)
	}
}

func TestTier2FailOpen(t *testing.T) {
	d := tier2FailOpen("tier-2 credentials unavailable")
	if !d.FailOpen || d.Source != sourceTier2FailOpen {
		t.Errorf("tier2FailOpen must be FailOpen + fail-open source, got %+v", d)
	}
	if d.Evaluation.Verdict != client.VerdictUnknown {
		t.Errorf("verdict = %q, want UNKNOWN", d.Evaluation.Verdict)
	}
	// The cause becomes the fail-closed reason via failClosedReason — content-free.
	if d.Evaluation.Reason != "tier-2 credentials unavailable" {
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

func TestResolveTier2Timeout(t *testing.T) {
	isolateConfig(t)
	if got := ResolveTier2Timeout(); got != defaultTier2Timeout {
		t.Errorf("default = %v, want %v", got, defaultTier2Timeout)
	}
	t.Setenv(envTier2Timeout, "1000")
	if got := ResolveTier2Timeout(); got != time.Second {
		t.Errorf("1000ms env = %v, want 1s", got)
	}
	// Over the clamp → maxTier2Timeout (correctness bound under the 5s hook timeout).
	t.Setenv(envTier2Timeout, "60000")
	if got := ResolveTier2Timeout(); got != maxTier2Timeout {
		t.Errorf("60000ms env = %v, want the %v clamp", got, maxTier2Timeout)
	}
	// Garbage env is ignored → default (never wipes a valid config silently).
	t.Setenv(envTier2Timeout, "notanumber")
	if got := ResolveTier2Timeout(); got != defaultTier2Timeout {
		t.Errorf("garbage env = %v, want default %v", got, defaultTier2Timeout)
	}
	// A margin invariant: the max T2 budget must stay under the shipped hook timeout.
	if maxTier2Timeout >= 5*time.Second {
		t.Errorf("maxTier2Timeout %v must stay under CC's 5s hook timeout (fails OPEN)", maxTier2Timeout)
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
	isolateConfig(t) // default T2 budget = defaultTier2Timeout (3.5s)
	// Just started: remaining (~4s) exceeds the default, so the budget is the default.
	if got := tier2Budget(time.Now()); got != defaultTier2Timeout {
		t.Errorf("fresh enforceStart budget = %v, want default %v", got, defaultTier2Timeout)
	}
	// T1 already consumed the whole-hook cap → the remainder (and thus the budget) is
	// non-positive, so escalateTier2 fail-opens immediately rather than overrun the hook.
	if got := tier2Budget(time.Now().Add(-maxEnforceHookBudget - time.Second)); got > 0 {
		t.Errorf("exhausted-cap budget = %v, want <= 0 (immediate fail-open)", got)
	}
	// Whole-hook cap must stay under CC's 5s hook timeout even with T1 at its clamp.
	if maxEnforceHookBudget >= 5*time.Second {
		t.Errorf("maxEnforceHookBudget %v must stay under the 5s hook timeout", maxEnforceHookBudget)
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

// tier2Creds points the hot-path credential resolver at a fake core with a valid
// signing seed (no secret store).
func tier2Creds(t *testing.T, baseURL string) {
	t.Helper()
	t.Setenv(envBaseURL, baseURL)
	t.Setenv(envAPIKeyDirect, testTier2Key)
	t.Setenv(envSeedDirect, testSeedB64)
	t.Setenv(envContentCapture, "0")
}

func TestEscalateTier2_RealVerdict(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	url, hits := serveEvaluate(t, `{"verdict":"block","reason":"tier2 exec policy","policy_id":"t2-pol"}`, 200, 0)
	tier2Creds(t, url)

	m := NewMapper(Identity{DeveloperDID: testDID})
	ev := &HookEvent{HookEventName: "PreToolUse", SessionID: "s", Cwd: "/tmp", ToolName: "Bash",
		ToolInput: []byte(`{"command":"rm -rf /tmp/x"}`)}
	dec := escalateTier2(context.Background(), log.New(&nopWriter{}, "", 0), m, ev, time.Second)

	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("/evaluate hits = %d, want 1", atomic.LoadInt32(hits))
	}
	if dec.FailOpen {
		t.Error("a real BLOCK from core must be FailOpen=false")
	}
	if dec.Evaluation.Verdict != client.VerdictBlock {
		t.Errorf("verdict = %q, want BLOCK", dec.Evaluation.Verdict)
	}
	if dec.Source != sourceTier2 {
		t.Errorf("Source = %q, want %q", dec.Source, sourceTier2)
	}
}

func TestEscalateTier2_Outage(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	// A 500 exhausts the client's retries and Emit fails open → no verdict.
	url, _ := serveEvaluate(t, `boom`, 500, 0)
	tier2Creds(t, url)

	m := NewMapper(Identity{DeveloperDID: testDID})
	ev := &HookEvent{HookEventName: "PreToolUse", SessionID: "s", Cwd: "/tmp", ToolName: "Bash",
		ToolInput: []byte(`{"command":"echo hi"}`)}
	dec := escalateTier2(context.Background(), log.New(&nopWriter{}, "", 0), m, ev, time.Second)
	if !dec.FailOpen {
		t.Error("a /evaluate outage must yield a fail-open decision")
	}
	if dec.Source != sourceTier2FailOpen {
		t.Errorf("Source = %q, want %q", dec.Source, sourceTier2FailOpen)
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
	dec := escalateTier2(context.Background(), log.New(&nopWriter{}, "", 0), m, ev, time.Second)
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
	// A server slower than the budget: escalateTier2 must return within ~budget
	// (the ctx cancels Emit), not wait for the server.
	url, _ := serveEvaluate(t, `{"verdict":"allow"}`, 200, 2*time.Second)
	tier2Creds(t, url)
	m := NewMapper(Identity{DeveloperDID: testDID})
	ev := &HookEvent{HookEventName: "PreToolUse", SessionID: "s", Cwd: "/tmp", ToolName: "Bash",
		ToolInput: []byte(`{"command":"echo hi"}`)}
	start := time.Now()
	dec := escalateTier2(context.Background(), log.New(&nopWriter{}, "", 0), m, ev, 150*time.Millisecond)
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Errorf("escalateTier2 took %v, must return near the 150ms budget", elapsed)
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
		socket, _ := serveSidecar(t, &sidecar.Bundle{Version: "t2-allow", DefaultDecision: "allow"})
		t.Setenv(envSidecarSocket, socket)
	}

	t.Run("C12 T1 allow + T2 BLOCK on Bash → deny (floor closed)", func(t *testing.T) {
		allowT1(t)
		url, hits := serveEvaluate(t, `{"verdict":"block","reason":"tier2 exec policy","policy_id":"t2-pol"}`, 200, 0)
		tier2Creds(t, url)
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
		tier2Creds(t, url)
		t.Setenv(envTier2, "1")
		t.Setenv(envFailClosed, "0")
		if out := run(benignBash); strings.TrimSpace(out) != "" {
			t.Errorf("a real T2 ALLOW must proceed (empty stdout); got %q", out)
		}
		if atomic.LoadInt32(hits) != 1 {
			t.Errorf("/evaluate hits = %d, want exactly 1", atomic.LoadInt32(hits))
		}
	})

	t.Run("C14 T2 NOT escalated for a non-high-risk class (Edit)", func(t *testing.T) {
		allowT1(t)
		url, hits := serveEvaluate(t, `{"verdict":"block","reason":"should not fire"}`, 200, 0)
		tier2Creds(t, url)
		t.Setenv(envTier2, "1")
		t.Setenv(envFailClosed, "0")
		if out := run(editCall); strings.TrimSpace(out) != "" {
			t.Errorf("Edit is T1-only; must not be blocked by T2; got %q", out)
		}
		if atomic.LoadInt32(hits) != 0 {
			t.Errorf("/evaluate must NOT be called for Edit; hits=%d", atomic.LoadInt32(hits))
		}
	})

	t.Run("C15 T2 outage: fail-open proceeds / fail-closed denies within bound", func(t *testing.T) {
		allowT1(t)
		url, _ := serveEvaluate(t, `boom`, 500, 0) // exhausts retries → Emit fails open
		tier2Creds(t, url)
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

	t.Run("C16 T2 OFF → high-risk Bash never calls /evaluate (T1-only)", func(t *testing.T) {
		allowT1(t)
		url, hits := serveEvaluate(t, `{"verdict":"block"}`, 200, 0)
		tier2Creds(t, url)
		t.Setenv(envTier2, "0") // opt-out
		t.Setenv(envFailClosed, "0")
		if out := run(dangerBash); strings.TrimSpace(out) != "" {
			t.Errorf("T2 off must be byte-identical to T1-only (proceed); got %q", out)
		}
		if atomic.LoadInt32(hits) != 0 {
			t.Errorf("T2 off must never call /evaluate; hits=%d", atomic.LoadInt32(hits))
		}
	})

	t.Run("C17 T1 already denies → T2 short-circuited (no /evaluate)", func(t *testing.T) {
		// A T1 BLOCK bundle denies the rm -rf; T2 must not fire (governance only
		// tightens — there is nothing for T2 to add to a block).
		socket, _ := serveSidecar(t, blockRuleBundle())
		t.Setenv(envSidecarSocket, socket)
		url, hits := serveEvaluate(t, `{"verdict":"allow"}`, 200, 0)
		tier2Creds(t, url)
		t.Setenv(envTier2, "1")
		t.Setenv(envFailClosed, "0")
		out := run(dangerBash)
		d, reason := parsePermissionDecision(t, []byte(out))
		if d != ccDecisionDeny {
			t.Fatalf("T1 BLOCK must deny; got %q (stdout=%q)", d, out)
		}
		if !strings.Contains(reason, "destructive recursive delete") {
			t.Errorf("reason = %q, want the T1 policy reason (T2 not consulted)", reason)
		}
		if atomic.LoadInt32(hits) != 0 {
			t.Errorf("T2 must be short-circuited when T1 denies; /evaluate hits=%d", atomic.LoadInt32(hits))
		}
	})
}
