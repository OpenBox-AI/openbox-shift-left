package hookflow

import (
	"encoding/json"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// testContract is a minimal OutputContract standing in for a provider. It maps
// REQUIRE_APPROVAL to `ask`, as Claude Code does; the Codex mapping (`deny`) is
// covered by the escalation cases below, which are contract-independent.
type testContract struct{ approval string }

func (c testContract) ApprovalDecision() string { return c.approval }
func (testContract) ContentFieldKeys() []string { return nil }
func (testContract) Render(d, reason string, updated json.RawMessage) ([]byte, string) {
	if d == "" {
		return nil, ""
	}
	return []byte(d), d
}

func verdictDecision(v client.Verdict) decision.Decision {
	return decision.Decision{Evaluation: client.Evaluation{Verdict: v}}
}

func TestShouldEscalate(t *testing.T) {
	for _, tc := range []struct {
		name string
		dec  decision.Decision
		want bool
	}{
		{"allow escalates — Tier-1 would proceed", verdictDecision(client.VerdictAllow), true},
		{"unknown escalates — no local verdict", verdictDecision(client.VerdictUnknown), true},
		{"halt does not — already a final answer", verdictDecision(client.VerdictHalt), false},
		{"block does not — already a final answer", verdictDecision(client.VerdictBlock), false},
		// E9 §3.4 Step 0: REQUIRE_APPROVAL is a question, not an answer. Without
		// this the request is never filed with the server and no approver ever
		// sees it.
		{"require_approval escalates despite tightening", verdictDecision(client.VerdictRequireApproval), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, c := range []testContract{{approval: "ask"}, {approval: DecisionDeny}} {
				if got := ShouldEscalate(tc.dec, c); got != tc.want {
					t.Errorf("ShouldEscalate(approval=%q) = %t, want %t", c.approval, got, tc.want)
				}
			}
		})
	}
}

func TestKeepTighterHoldsTheTier1Floor(t *testing.T) {
	c := testContract{approval: "ask"}
	t1 := verdictDecision(client.VerdictRequireApproval)

	// A degraded escalation must not loosen a Tier-1 deny/ask.
	if got := KeepTighter(t1, Tier2FailOpen("undelivered"), c); got.Evaluation.Verdict != client.VerdictRequireApproval {
		t.Errorf("fail-open Tier-2 replaced the Tier-1 floor with %q", got.Evaluation.Verdict)
	}
	// A real Tier-2 answer wins, including a looser one — the server is
	// authoritative once it actually answers.
	allow := decision.Decision{Evaluation: client.Evaluation{Verdict: client.VerdictAllow}, Source: SourceTier2}
	if got := KeepTighter(t1, allow, c); got.Evaluation.Verdict != client.VerdictAllow {
		t.Errorf("real Tier-2 verdict was discarded: got %q", got.Evaluation.Verdict)
	}
	// Nothing to protect when Tier-1 would have proceeded anyway.
	if got := KeepTighter(verdictDecision(client.VerdictAllow), Tier2FailOpen("undelivered"), c); !got.FailOpen {
		t.Error("a would-proceed Tier-1 must not be preferred over the fail-open Tier-2 marker")
	}
}
