package claudecode

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

// maxTier2Timeout caps the configurable T2 budget. Unchanged by the raised hook
// ceiling below: a slow control plane must still degrade quickly, and only a
// FILED approval is worth waiting longer for.
const maxTier2Timeout = 4 * time.Second

// preToolUseHookTimeoutSec is the `timeout` the plugin installs on PreToolUse
// (plugin/hooks/hooks.json), and the value every enforce budget below derives
// from — so raising one raises them in lock-step and the two cannot drift
// (installer_test pins the JSON to this constant).
//
// PreToolUse alone carries the raised ceiling because it is the only gating
// hook, and so the only one that can hold for an approval decision (E9-S4);
// every other hook keeps 5s. Within PreToolUse the narrowing to high-risk
// classes is the engine's job, not the matcher's: only shell/MCP escalate, and
// only an escalation can hold.
const preToolUseHookTimeoutSec = 30

// otherHookTimeoutSec is the `timeout` installed on every non-gating hook. The
// tightest of them, declared as the ceiling for all: SessionEnd gets 15s, but a
// budget derived from the shortest is the one that holds everywhere.
const otherHookTimeoutSec = 5

// HookCeilings declares what Claude Code kills a hook at, so hookflow derives
// its own budget (EnforceBudget subtracts the engine's margin). The values are
// the ones our own installer writes — localhooks.go for a project-scope install,
// the plugin bundle's hooks/hooks.json for the plugin — because the installed
// timeout, not the tool's default, is what actually kills the hook.
func (Engine) HookCeilings() providerspi.HookCeiling {
	return providerspi.HookCeiling{
		Gating: time.Duration(preToolUseHookTimeoutSec) * time.Second,
		Other:  otherHookTimeoutSec * time.Second,
	}
}

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
