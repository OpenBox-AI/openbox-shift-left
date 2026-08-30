package hookflow

import (
	"encoding/json"

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

// TestShouldEscalate is deleted with ShouldEscalate.
//
// The predicate answered "can a round-trip still change the outcome", which was
// a real question while a local step produced verdicts. Escalation is
// unconditional now: every gated class goes to the server because risk is a
// property of the policy, so the gate has nothing left to ask.

// TestKeepTighterHoldsTheTier1Floor is deleted with KeepTighter.
//
// It covered the one thing that function existed for: a degraded evaluation
// must not replace a LOCAL deny/ask with VerdictUnknown and let the call
// through — enforcement loosening itself on an outage. That cannot happen now,
// because the local step produces no verdict to loosen. What protects the same
// direction today is ApplyFailurePolicy: a degraded evaluation carries
// FailOpen, and a fail-closed org denies on it. TestApplyFailurePolicy covers
// that, and the C15 conformance case covers it end to end.
