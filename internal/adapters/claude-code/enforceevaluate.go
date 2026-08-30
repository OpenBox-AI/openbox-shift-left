package claudecode

import (
	"context"
	"log"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
	providerspi "github.com/openbox-ai/openbox-shift-left/internal/provider"
)

const bashToolName = "Bash"

// maxEvaluationTimeout unchanged by the raised hook ceiling below: a slow
// control plane must still degrade quickly, and only a filed approval is worth
// waiting longer for.
const maxEvaluationTimeout = 4 * time.Second

const preToolUseHookTimeoutSec = 30

const otherHookTimeoutSec = 5

// HookCeilings declares what Claude Code kills a hook at, so hookflow derives
// its own budget (EnforceBudget subtracts the engine's margin).
func (Engine) HookCeilings() providerspi.HookCeiling {
	return providerspi.HookCeiling{
		Gating: time.Duration(preToolUseHookTimeoutSec) * time.Second,
		Other:  otherHookTimeoutSec * time.Second,
	}
}

var evaluator = hookflow.Evaluator{
	Ceiling:    Engine{}.HookCeilings(),
	MaxTimeout: maxEvaluationTimeout,
	NewClient: func(logger *log.Logger) (hookflow.Governor, error) {
		creds, err := ResolveCredentials()
		if err != nil {
			return nil, err
		}
		return creds.NewClient(logger)
	},
}

func evaluationBudget(enforceStart time.Time) time.Duration {
	return evaluator.Budget(enforceStart, ResolveEvaluationTimeout())
}

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

func escalateEvaluation(ctx context.Context, logger *log.Logger, m Mapper, ev *HookEvent, budget time.Duration) decision.Decision {
	devEv, ok := m.Map(HookPreToolUse, ev)
	if !ok {
		return hookflow.EvaluationFailOpen("event not mappable")
	}
	return evaluator.Escalate(ctx, logger, devEv, budget)
}
