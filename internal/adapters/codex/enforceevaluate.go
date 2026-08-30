package codex

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

// HookCeilings declares what Codex kills a hook at.
func (Engine) HookCeilings() providerspi.HookCeiling {
	return providerspi.HookCeiling{
		Gating: time.Duration(preToolUseHookTimeoutSec) * time.Second,
		Other:  time.Duration(hotHookTimeoutSec) * time.Second,
	}
}

var maxEvaluationTimeout = hookflow.EnforceBudget(Engine{}.HookCeilings())

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
