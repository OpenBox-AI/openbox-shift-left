package codex

import (
	"context"
	"log"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

const bashToolName = "Bash"

// maxTier2Timeout caps the configurable T2 budget at the whole-hook ceiling;
// unlike Claude Code's fixed 5s kill, Codex's is derived from the timeout this
// installer wrote into hooks.json.
const maxTier2Timeout = maxEnforceHookBudget

// tier2 binds the shared escalation to this provider's hook budget and
// transport. The escalation itself — bounded run, degrade-on-timeout, verdict
// mapping — is provider-independent and lives in hookflow.
var tier2 = hookflow.Tier2{
	HookBudget: maxEnforceHookBudget,
	MaxTimeout: maxTier2Timeout,
	NewClient: func(logger *log.Logger) (hookflow.Governor, error) {
		creds, err := ResolveCredentials()
		if err != nil {
			return nil, err
		}
		return creds.NewClient(logger)
	},
}

// tier2Budget is the effective budget for an escalation given when the enforce
// block began.
func tier2Budget(enforceStart time.Time) time.Duration {
	return tier2.Budget(enforceStart, ResolveTier2Timeout())
}

// isHighRiskClass selects the tool classes worth a synchronous round-trip:
// shell execution and MCP calls, where a wrong local allow is most costly.
func isHighRiskClass(toolName string) bool {
	if toolName == bashToolName {
		return true
	}
	kind, _, _, _, _ := classifyTool(toolName)
	return kind == client.ToolMCP
}

func decisionTightens(dec decision.Decision) bool {
	return hookflow.DecisionTightens(dec, contract)
}

// escalateTier2 maps the native hook event and runs the shared escalation. The
// mapping is the only provider-specific step.
func escalateTier2(ctx context.Context, logger *log.Logger, m Mapper, ev *HookEvent, budget time.Duration) decision.Decision {
	devEv, ok := m.Map(HookPreToolUse, ev)
	if !ok {
		return hookflow.Tier2FailOpen("tier-2 event not mappable")
	}
	return tier2.Escalate(ctx, logger, devEv, budget)
}
