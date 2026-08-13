package codex

import (
	"context"
	"log"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
	providerspi "github.com/openbox-ai/openbox-shift-left/provider"
)

const bashToolName = "Bash"

// HookCeilings declares what Codex kills a hook at. Codex's own default is 600s,
// but the ceiling that matters is the `timeout` this installer writes on each
// handler — that is the value Codex enforces, and it is what an org changes if
// it wants more headroom. Declaring the installed value keeps the engine's
// budget correct without a second edit when it moves.
func (Engine) HookCeilings() providerspi.HookCeiling {
	return providerspi.HookCeiling{
		Gating: time.Duration(preToolUseHookTimeoutSec) * time.Second,
		Other:  time.Duration(hotHookTimeoutSec) * time.Second,
	}
}

// maxTier2Timeout caps the configurable evaluation budget at the whole-hook
// budget derived from the declared ceiling.
var maxTier2Timeout = hookflow.EnforceBudget(Engine{}.HookCeilings())

// tier2 binds the shared escalation to this provider's declared ceiling and
// transport. The escalation itself — bounded run, degrade-on-timeout, verdict
// mapping — is provider-independent and lives in hookflow.
var tier2 = hookflow.Tier2{
	Ceiling:    Engine{}.HookCeilings(),
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
