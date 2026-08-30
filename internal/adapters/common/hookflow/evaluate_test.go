package hookflow

import (
	"encoding/json"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
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

// TestKeepTighterHoldsTheTier1Floor is deleted with KeepTighter.
//
// It covered the one thing that function existed for: a degraded evaluation
// must not replace a LOCAL deny/ask with VerdictUnknown and let the call
// through — enforcement loosening itself on an outage. That cannot happen now,
// because the local step produces no verdict to loosen. What protects the same
// direction today is ApplyFailurePolicy: a degraded evaluation carries
// FailOpen, and a fail-closed org denies on it. TestApplyFailurePolicy covers
// that, and the C15 conformance case covers it end to end.
