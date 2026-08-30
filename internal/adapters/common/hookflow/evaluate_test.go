package hookflow

import (
	"encoding/json"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

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

// TestKeepTighterHoldsTheTier1Floor is deleted with KeepTighter. It covered
// the one thing that function existed for: a degraded evaluation must not
// replace a local deny/ask with VerdictUnknown and let the call through;
// enforcement loosening itself on an outage.
