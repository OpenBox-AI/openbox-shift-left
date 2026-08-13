package codex

import (
	"context"
	"log"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

func TestIsHighRiskClass_Codex(t *testing.T) {
	cases := map[string]bool{
		"Bash":                   true,  // arbitrary shell
		"mcp__github__create_pr": true,  // MCP execution
		"apply_patch":            false, // file edit — T1 only (no /evaluate latency)
		"web_search":             false, // catch-all shell, not arbitrary exec
		"update_plan":            false,
	}
	for tool, want := range cases {
		if got := isHighRiskClass(tool); got != want {
			t.Errorf("isHighRiskClass(%q) = %v, want %v", tool, got, want)
		}
	}
}

func TestDecisionTightens_Codex(t *testing.T) {
	block := decision.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictBlock}}
	approval := decision.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictRequireApproval}}
	allow := decision.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictAllow}}
	if !decisionTightens(block) {
		t.Error("BLOCK must count as tightening (T2 skipped)")
	}
	if !decisionTightens(approval) {
		t.Error("REQUIRE_APPROVAL→deny must count as tightening on Codex (OD-SL7-ASK)")
	}
	if decisionTightens(allow) {
		t.Error("ALLOW must NOT tighten (T2 fires)")
	}
}

func TestEvaluationDecision_Codex(t *testing.T) {
	if d := hookflow.EvaluationDecision(client.Evaluation{Verdict: client.VerdictUnknown}); !d.FailOpen {
		t.Error("an unknown T2 verdict must fold to hookflow.FailOpen (no real verdict)")
	}
	if d := hookflow.EvaluationDecision(client.Evaluation{Verdict: client.VerdictBlock}); d.FailOpen {
		t.Error("a real T2 BLOCK must be hookflow.FailOpen=false")
	}
}

func TestTier2Budget_ClampsUnderWholeHookBudget(t *testing.T) {
	// A T1 gate that already consumed most of the budget leaves T2 a smaller slice.
	start := time.Now().Add(-(hookflow.EnforceBudget((Engine{}).HookCeilings()) - 200*time.Millisecond))
	if b := evaluationBudget(start); b > 300*time.Millisecond {
		t.Errorf("evaluationBudget = %v, want it clamped to the remaining whole-hook budget", b)
	}
	// A T1 gate that overran the whole budget yields a non-positive T2 budget →
	// escalateEvaluation fail-opens immediately (safe direction).
	over := time.Now().Add(-2 * hookflow.EnforceBudget((Engine{}).HookCeilings()))
	if b := evaluationBudget(over); b > 0 {
		t.Errorf("evaluationBudget after overrun = %v, want non-positive (immediate fail-open)", b)
	}
}

func TestEscalateTier2_DegradesFailOpenOnZeroBudget(t *testing.T) {
	// With a non-positive budget the whole escalation must degrade to a fail-open
	// decision (never hang, never a real verdict) so the failure policy decides.
	m := NewMapper(Identity{DeveloperDID: testDID})
	ev := &HookEvent{SessionID: "s", ToolName: "Bash", ToolInput: []byte(`{"command":"echo hi"}`)}
	dec := escalateEvaluation(context.Background(), log.New(&nopWriter{}, "", 0), m, ev, -1)
	if !dec.FailOpen {
		t.Errorf("zero/negative budget must degrade fail-open, got %+v", dec)
	}
}

// TestTier2EventIDMatchesObserve pins Sam G_SEC F2: the Tier-2 /evaluate copy of a
// PreToolUse event must derive the SAME deterministic event_id as its spooled
// observe counterpart, so the two collapse under one Idempotency-Key server-side
// (no double-count). RunHook pins ad.Mapper.Now to one instant and hands that SAME
// pinned Mapper to escalateEvaluation → runTier2, so both Map() calls fold the identical
// RFC3339Nano timestamp into deriveID.
func TestTier2EventIDMatchesObserve(t *testing.T) {
	ev := &HookEvent{
		SessionID: "s", PermissionMode: "default", ToolName: "Bash",
		ToolUseID: "call-1", ToolInput: []byte(`{"command":"deploy prod"}`),
	}

	// A pinned Mapper (as RunHook builds it) maps the SAME logical event twice — the
	// observe/spool copy and the Tier-2 copy — and must yield identical event_ids.
	pinned := time.Now()
	m := NewMapper(Identity{DeveloperDID: testDID})
	m.Now = func() time.Time { return pinned }

	observeEv, ok := m.Map(HookPreToolUse, ev)
	if !ok {
		t.Fatal("observe map failed")
	}
	tier2Ev, ok := m.Map(HookPreToolUse, ev) // what runTier2 does with the same pinned Mapper
	if !ok {
		t.Fatal("tier-2 map failed")
	}
	if observeEv.EventID == "" || tier2Ev.EventID != observeEv.EventID {
		t.Fatalf("pinned Mapper must derive one event_id: observe=%q tier2=%q", observeEv.EventID, tier2Ev.EventID)
	}

	// Guard the regression the fix addresses: an UNPINNED Mapper (fresh time.Now on
	// each Map) diverges — the exact double-count Sam F2 flagged.
	un := NewMapper(Identity{DeveloperDID: testDID})
	a, _ := un.Map(HookPreToolUse, ev)
	time.Sleep(2 * time.Millisecond) // ensure a distinct RFC3339Nano instant
	b, _ := un.Map(HookPreToolUse, ev)
	if a.EventID == b.EventID {
		t.Skip("clock resolution too coarse to demonstrate divergence; the pinned-equality assertion above is the load-bearing one")
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
